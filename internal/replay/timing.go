package replay

import (
	"math/rand"
	"time"

	"github.com/hkocamandev/pitchlab/internal/events"
)

// Statcast records a calendar date, not a clock time, so a replayed pitch has
// no timestamp to reuse. The simulator synthesizes one.
//
// These values follow the pitch timer introduced in 2023 -- 15 seconds with
// the bases empty, 18 with a runner on -- plus the time a pitcher actually
// takes to come set, which puts observed intervals closer to 18 to 24 seconds.
// The aim is a stream that looks like a game to a dashboard, not a claim to
// reproduce the real clock.
const (
	// GapBasesEmpty and GapRunnersOn are the typical intervals between
	// pitches, including the pitcher's routine rather than the timer alone.
	GapBasesEmpty = 19 * time.Second
	GapRunnersOn  = 23 * time.Second

	// TwoStrikeExtra reflects pitchers working more deliberately with the
	// at-bat on the line.
	TwoStrikeExtra = 2 * time.Second

	// AtBatBreak is the pause as a new batter steps in.
	AtBatBreak = 25 * time.Second

	// HalfInningBreak is the change of sides. Two and a half minutes is the
	// commercial break, which dominates the gap.
	HalfInningBreak = 150 * time.Second

	// JitterMin and JitterMax bound the random variation applied to each gap.
	// Without it every interval would be identical, which reads as machinery
	// rather than a game.
	JitterMin = -2 * time.Second
	JitterMax = 4 * time.Second

	// FirstPitchHour and FirstPitchMinute set a plausible start of play.
	FirstPitchHour   = 19
	FirstPitchMinute = 10
)

// Clock produces synthetic pitch times for one outing.
//
// Seeded explicitly so a replay is reproducible: the same tape and the same
// seed produce the same timestamps, which is what makes a demo repeatable and
// a timing test possible at all.
type Clock struct {
	rng     *rand.Rand
	current time.Time

	lastAtBat  int
	lastInning int
	started    bool
}

// NewClock starts a clock for an outing.
//
// The start time is derived from the source game date plus a plausible first
// pitch, advanced by the innings that had already been played when this
// pitcher entered. A reliever who came in during the seventh should not appear
// to have started the game.
func NewClock(sourceGameDate string, firstInning int, seed int64) *Clock {
	day := parseDate(sourceGameDate)

	start := time.Date(day.Year(), day.Month(), day.Day(),
		FirstPitchHour, FirstPitchMinute, 0, 0, time.UTC)
	if firstInning > 1 {
		start = start.Add(time.Duration(firstInning-1) * 20 * time.Minute)
	}

	return &Clock{
		rng:        rand.New(rand.NewSource(seed)),
		current:    start,
		lastInning: firstInning,
	}
}

// Next returns the timestamp for the next pitch.
//
// The interval depends on the game state, because that is what actually
// governs pace: runners on base slow a pitcher down, a new batter takes time
// to settle, and a change of sides is a different order of magnitude.
func (c *Clock) Next(ctx events.PitchContext, atBatNumber int) time.Time {
	if !c.started {
		c.started = true
		c.lastAtBat = atBatNumber
		c.lastInning = ctx.Inning
		return c.current
	}

	gap := GapBasesEmpty
	if ctx.On1B || ctx.On2B || ctx.On3B {
		gap = GapRunnersOn
	}
	if ctx.Strikes >= 2 {
		gap += TwoStrikeExtra
	}

	switch {
	case ctx.Inning != c.lastInning:
		gap += HalfInningBreak
	case atBatNumber != c.lastAtBat:
		gap += AtBatBreak
	}

	gap += c.jitter()
	if gap < time.Second {
		gap = time.Second
	}

	c.current = c.current.Add(gap)
	c.lastAtBat = atBatNumber
	c.lastInning = ctx.Inning
	return c.current
}

func (c *Clock) jitter() time.Duration {
	span := int64(JitterMax - JitterMin)
	return JitterMin + time.Duration(c.rng.Int63n(span+1))
}

func parseDate(s string) time.Time {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	// A tape without a date still replays; the timestamps are synthetic
	// either way, and refusing the whole outing over a missing field would be
	// worse than dating it today.
	return time.Now().UTC().Truncate(24 * time.Hour)
}
