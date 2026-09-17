package kafka

import (
	"errors"
	"fmt"
	"testing"

	kafka "github.com/segmentio/kafka-go"
)

// The error class travels with the dead letter and is what triage reads: the
// replay tool filters on it and the DLQ metric is labelled by it. A letter
// filed under the wrong class is a letter nobody finds.

func TestAConstraintViolationIsClassifiedAsPermanent(t *testing.T) {
	err := fmt.Errorf("insert pitch: %w", errors.New(
		`ERROR: insert or update on table "pitches" violates foreign key `+
			`constraint "pitches_batter_athlete_id_fkey" (SQLSTATE 23503)`))

	if got := Classify(err); got != ClassPermanent {
		t.Fatalf("Classify = %v, want ClassPermanent", got)
	}
}

func TestTheDeadLetterRecordsWhyItFailed(t *testing.T) {
	// A database integrity violation arrives from the store as an ordinary
	// wrapped error, not as a PermanentError: Classify recognises it on the
	// SQLSTATE alone. The class written into the letter has to recognise it
	// the same way, or the consumer dead-letters a message it understood
	// perfectly and then files it under "UNKNOWN".
	err := fmt.Errorf("insert pitch: %w", errors.New(
		`ERROR: insert or update on table "pitches" violates foreign key `+
			`constraint "pitches_batter_athlete_id_fkey" (SQLSTATE 23503)`))

	if got := errorClass(err); got != "PERSISTENCE_PERMANENT" {
		t.Fatalf("errorClass = %q, want %q", got, "PERSISTENCE_PERMANENT")
	}
}

func TestAWrappedPermanentErrorKeepsItsSpecificClass(t *testing.T) {
	// The explicit case still has to win: a PermanentError raised for a
	// business rule must not be relabelled by a stray SQLSTATE in its text.
	err := &PermanentError{Err: errors.New("pitch belongs to a closed session")}

	if got := errorClass(err); got != "BUSINESS_RULE_VIOLATION" {
		t.Fatalf("errorClass = %q, want %q", got, "BUSINESS_RULE_VIOLATION")
	}
}

func TestAnUnrecognisedFailureIsStillUnknown(t *testing.T) {
	// "UNKNOWN" has to keep meaning something. If everything ends up here the
	// label stops being a triage signal.
	if got := errorClass(errors.New("something nobody predicted")); got != "UNKNOWN" {
		t.Fatalf("errorClass = %q, want %q", got, "UNKNOWN")
	}
}

// --- replay cycles ---------------------------------------------------------

func TestReplayCountIsReadFromTheMessage(t *testing.T) {
	// The replay tool stamps this header when it puts a letter back on the
	// source topic. If the consumer does not carry it into the next dead
	// letter, a permanently-broken message can be replayed for ever: it fails,
	// is filed again with no memory of having been replayed, and the next
	// drain picks it up as if it were new. Thirty letters became twelve
	// hundred that way in ten minutes.
	cases := []struct {
		name    string
		headers []kafka.Header
		want    int
	}{
		{"no header at all", nil, 0},
		{"a first replay", []kafka.Header{{Key: ReplayCountHeader, Value: []byte("1")}}, 1},
		{"a later replay", []kafka.Header{{Key: ReplayCountHeader, Value: []byte("7")}}, 7},
		{"nonsense is not a count", []kafka.Header{{Key: ReplayCountHeader, Value: []byte("x")}}, 0},
		{"negative is not a count", []kafka.Header{{Key: ReplayCountHeader, Value: []byte("-3")}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayCount(tc.headers); got != tc.want {
				t.Fatalf("replayCount = %d, want %d", got, tc.want)
			}
		})
	}
}
