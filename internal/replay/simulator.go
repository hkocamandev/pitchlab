package replay

import (
	"context"
	"fmt"
	"time"

	"github.com/hkocamandev/pitchlab/internal/events"
)

// DeviceKey and DeviceKind identify the simulator as a measurement source.
//
// Registering as a device is what makes replayed rows distinguishable from
// real ones at the database level, rather than by convention or by reading
// the logs.
const (
	DeviceKey  = "replay-sim-01"
	DeviceKind = "REPLAY_SIMULATOR"
)

// Config tunes a replay run.
type Config struct {
	// Speed multiplies real time. 1 replays at the pace of a game, 30 turns
	// an outing into about a minute, and 0 means no waiting at all -- useful
	// for load testing and for filling a database quickly.
	Speed float64

	// Seed makes a run reproducible. The same tape and seed produce the same
	// timestamps, which is what allows a demo to be rehearsed and a timing
	// test to assert anything.
	Seed int64

	// Producer is the version string recorded in each envelope.
	Producer string
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{Speed: 30, Seed: 42, Producer: "pitchlab-replay/dev"}
}

// Emitter publishes one event.
//
// An interface rather than a concrete producer so the simulator can be tested
// without a broker, and so nothing in this package needs to know what Kafka
// is.
type Emitter interface {
	Emit(ctx context.Context, env *events.Envelope) error
}

// EmitterFunc adapts a function to an Emitter.
type EmitterFunc func(ctx context.Context, env *events.Envelope) error

// Emit calls the function.
func (f EmitterFunc) Emit(ctx context.Context, env *events.Envelope) error {
	return f(ctx, env)
}

// Progress reports a replayed pitch to a caller that wants to show it.
type Progress func(session string, index, total int, at time.Time)

// Simulator replays a tape as a stream of device events.
type Simulator struct {
	cfg      Config
	emitter  Emitter
	progress Progress
	sleep    func(context.Context, time.Duration) error
}

// New builds a simulator.
func New(cfg Config, emitter Emitter) *Simulator {
	if cfg.Producer == "" {
		cfg.Producer = "pitchlab-replay/dev"
	}
	return &Simulator{cfg: cfg, emitter: emitter, sleep: sleepCtx}
}

// OnProgress registers a per-pitch callback.
func (s *Simulator) OnProgress(fn Progress) { s.progress = fn }

// Stats summarizes a completed run.
type Stats struct {
	Sessions int
	Pitches  int
	Elapsed  time.Duration
	// SimulatedSpan is how much game time the replayed pitches covered.
	//
	// Summed per outing rather than taken as the overall min-to-max: outings
	// come from different calendar dates, so the overall span would include
	// the days between games and report a pacing ratio in the hundreds of
	// thousands. Summing the individual outings measures what was actually
	// waited for.
	SimulatedSpan time.Duration
}

// Run replays the tape.
//
// Sessions are replayed one after another rather than interleaved. Real games
// overlap, but a demo reads better when one outing plays out at a time, and
// nothing downstream depends on the interleaving: ordering is guaranteed per
// session by the partition key, not by the order sessions are emitted in.
func (s *Simulator) Run(ctx context.Context, tape *Tape) (Stats, error) {
	start := time.Now()
	stats := Stats{Sessions: len(tape.Sessions)}

	for _, session := range tape.Sessions {
		if len(session.Pitches) == 0 {
			continue
		}

		// Derive the per-session seed from the run seed and the session key,
		// so adding or removing an outing does not change the timestamps of
		// the others.
		clock := NewClock(session.SourceGameDate,
			session.Pitches[0].Context.Inning,
			s.cfg.Seed+int64(hashString(session.UID)))

		var previous, sessionStart time.Time

		for i, rec := range session.Pitches {
			at := clock.Next(rec.Context, rec.AtBatNumber)

			if i > 0 {
				if err := s.pace(ctx, at.Sub(previous)); err != nil {
					return stats, err
				}
			}
			previous = at

			env, err := s.buildEvent(rec, at)
			if err != nil {
				return stats, err
			}
			if err := s.emitter.Emit(ctx, env); err != nil {
				return stats, fmt.Errorf("emit %s: %w", rec.PitchUID(), err)
			}

			stats.Pitches++
			if i == 0 {
				sessionStart = at
			}
			if s.progress != nil {
				s.progress(session.UID, i+1, len(session.Pitches), at)
			}
		}

		stats.SimulatedSpan += previous.Sub(sessionStart)
	}

	stats.Elapsed = time.Since(start)
	return stats, nil
}

// pace waits out the scaled interval between two pitches.
func (s *Simulator) pace(ctx context.Context, gap time.Duration) error {
	if s.cfg.Speed <= 0 {
		// Burst mode: no waiting. Kafka absorbs the resulting spike, which is
		// exactly the property a buffer exists to provide.
		return nil
	}
	scaled := time.Duration(float64(gap) / s.cfg.Speed)
	if scaled <= 0 {
		return nil
	}
	return s.sleep(ctx, scaled)
}

// buildEvent wraps one tape record as a device event.
//
// The measurement is passed through untouched. Normalization is a domain rule
// and belongs in the processor, where it is tested against the training code;
// a tracking camera knows nothing about handedness conventions.
func (s *Simulator) buildEvent(rec TapeRecord, at time.Time) (*events.Envelope, error) {
	payload := events.PitchRaw{
		DeviceID:        DeviceKey,
		DeviceKind:      DeviceKind,
		ExternalGameRef: rec.ExternalGameRef,
		AtBatNumber:     rec.AtBatNumber,
		PitchNumber:     rec.PitchNumber,
		ThrownAt:        at,
		SourceGameDate:  rec.SourceGameDate,
		Session:         rec.Session,
		Batter:          rec.Batter,
		Context:         rec.Context,
		Measurement:     rec.Measurement,
		GroundTruth:     rec.GroundTruth,
	}

	env, err := events.NewEnvelope(
		events.TypePitchRaw,
		s.cfg.Producer,
		// Derived from the natural key, never random. A rerun of the same
		// tape has to look like a redelivery to the consumer, not like a new
		// set of pitches.
		events.IdempotencyKeyFor(rec.PitchUID()),
		// Keyed by session, so one outing lands on one partition and is
		// therefore consumed in order.
		rec.Session.SessionUID,
		at,
		payload,
	)
	if err != nil {
		return nil, err
	}

	// Marks every row this produces as replayed. The flag travels into the
	// session record, and the API surfaces it, so nobody mistakes a
	// simulation for live play.
	env.Synthetic = true
	return env, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hashString is a small FNV-1a, used only to spread session seeds.
func hashString(s string) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	h := uint32(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return h
}
