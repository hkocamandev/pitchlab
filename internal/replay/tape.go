// Package replay turns a tape of historical pitches into a stream of device
// events.
//
// The package imports neither the database nor the cache nor the inference
// client, and that is enforced by a test rather than by discipline. A real
// tracking camera does not write to your database; it emits events. If the
// simulator wrote directly, the Kafka to processor to inference to PostgreSQL
// backbone would never run during a demo, and normalization and idempotency
// would exist in two copies that inevitably diverge.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/hkocamandev/pitchlab/internal/events"
)

// TapeRecord is one line of a replay tape.
//
// It is deliberately the same shape as the device event, minus the timing,
// which the simulator synthesizes. Statcast records a calendar date, not a
// clock time, so there is nothing to replay from.
type TapeRecord struct {
	ExternalGameRef string `json:"external_game_ref"`
	AtBatNumber     int    `json:"at_bat_number"`
	PitchNumber     int    `json:"pitch_number"`
	SourceGameDate  string `json:"source_game_date"`

	Session     events.SessionRef     `json:"session"`
	Batter      events.BatterRef      `json:"batter"`
	Context     events.PitchContext   `json:"context"`
	Measurement events.RawMeasurement `json:"measurement"`
	GroundTruth *events.GroundTruth   `json:"ground_truth,omitempty"`
}

// PitchUID returns the pitch's natural key.
func (r TapeRecord) PitchUID() string {
	return events.PitchUID(r.ExternalGameRef, r.AtBatNumber, r.PitchNumber)
}

// Session is one pitcher's outing, with its pitches in order.
type Session struct {
	UID            string
	PitcherMLBAMID int
	GameRef        string
	SourceGameDate string
	Opponent       string
	Pitches        []TapeRecord
}

// Tape is a loaded set of outings.
type Tape struct {
	Sessions []Session
}

// PitchCount returns the total number of pitches on the tape.
func (t Tape) PitchCount() int {
	n := 0
	for _, s := range t.Sessions {
		n += len(s.Pitches)
	}
	return n
}

// LoadTape reads an NDJSON tape and groups it into outings.
//
// Grouping is by (game, pitcher), which is what a session means here: the
// same abstraction the WebSocket channel, the Kafka partition key and the
// anomaly baseline all use.
func LoadTape(path string) (*Tape, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tape: %w", err)
	}
	defer f.Close()
	return ReadTape(f)
}

// ReadTape parses an NDJSON tape from a reader.
func ReadTape(r io.Reader) (*Tape, error) {
	scanner := bufio.NewScanner(r)
	// Statcast rows carry a lot of floats; the default 64KB line limit is
	// ample, but a generous buffer costs nothing and avoids a confusing
	// truncation error if the format grows.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	byUID := map[string]*Session{}
	var order []string
	line := 0

	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}

		var rec TapeRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("tape line %d: %w", line, err)
		}
		if rec.Session.SessionUID == "" {
			return nil, fmt.Errorf("tape line %d: session_uid is required", line)
		}

		s, ok := byUID[rec.Session.SessionUID]
		if !ok {
			s = &Session{
				UID:            rec.Session.SessionUID,
				PitcherMLBAMID: rec.Session.PitcherMLBAMID,
				GameRef:        rec.ExternalGameRef,
				SourceGameDate: rec.SourceGameDate,
				Opponent:       rec.Session.OpponentTeam,
			}
			byUID[rec.Session.SessionUID] = s
			order = append(order, rec.Session.SessionUID)
		}
		s.Pitches = append(s.Pitches, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read tape: %w", err)
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("tape is empty")
	}

	tape := &Tape{Sessions: make([]Session, 0, len(order))}
	for _, uid := range order {
		s := byUID[uid]
		// Sort defensively. The exporter writes them in order, but a
		// hand-assembled tape should still replay correctly, and out-of-order
		// pitches would silently corrupt the session index.
		sort.SliceStable(s.Pitches, func(i, j int) bool {
			if s.Pitches[i].AtBatNumber != s.Pitches[j].AtBatNumber {
				return s.Pitches[i].AtBatNumber < s.Pitches[j].AtBatNumber
			}
			return s.Pitches[i].PitchNumber < s.Pitches[j].PitchNumber
		})
		tape.Sessions = append(tape.Sessions, *s)
	}
	return tape, nil
}

// Filter narrows a tape to the outings matching the given criteria.
//
// Zero values mean "no filter", so Filter(0, "", "") is the identity.
func (t *Tape) Filter(maxSessions int, pitcherMLBAMID string, gameRef string) *Tape {
	out := &Tape{}
	for _, s := range t.Sessions {
		if gameRef != "" && s.GameRef != gameRef {
			continue
		}
		if pitcherMLBAMID != "" &&
			fmt.Sprintf("%d", s.PitcherMLBAMID) != pitcherMLBAMID {
			continue
		}
		out.Sessions = append(out.Sessions, s)
		if maxSessions > 0 && len(out.Sessions) >= maxSessions {
			break
		}
	}
	return out
}
