// Package kafka wraps message production and consumption.
package kafka

import (
	"context"
	"errors"
	"net"
	"strings"
)

// Class says what should happen to a message that failed.
//
// Getting this classification right is most of what makes a consumer usable.
// Retrying a permanent error fails the same way five times and blocks the
// partition five times longer, while giving up on a transient one throws away
// work that would have succeeded a moment later.
type Class int

const (
	// ClassTransient is worth retrying: a dropped connection, a deadlock, a
	// dependency restarting.
	ClassTransient Class = iota
	// ClassPermanent will fail identically forever: malformed JSON, an
	// unknown schema version, a violated constraint. Straight to the DLQ.
	ClassPermanent
	// ClassDuplicate means the work was already done. Not an error at all;
	// at-least-once delivery guarantees it will happen.
	ClassDuplicate
)

func (c Class) String() string {
	switch c {
	case ClassTransient:
		return "transient"
	case ClassPermanent:
		return "permanent"
	case ClassDuplicate:
		return "duplicate"
	default:
		return "unknown"
	}
}

// PermanentError marks an error as not worth retrying.
type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps an error as permanent.
func Permanent(reason string, err error) error {
	return &PermanentError{Reason: reason, Err: err}
}

// ErrDuplicate reports that the message had already been processed.
var ErrDuplicate = errors.New("message already processed")

// Classify decides how to treat a processing error.
//
// The default is transient. That bias is deliberate: wrongly retrying a
// permanent error costs a few seconds of partition delay, while wrongly
// discarding a transient one silently loses a pitch.
func Classify(err error) Class {
	if err == nil {
		return ClassTransient
	}

	if errors.Is(err, ErrDuplicate) {
		return ClassDuplicate
	}

	var perm *PermanentError
	if errors.As(err, &perm) {
		return ClassPermanent
	}

	// A cancelled context means shutdown, not a bad message.
	if errors.Is(err, context.Canceled) {
		return ClassTransient
	}

	// Network trouble is the definition of transient.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ClassTransient
	}

	// PostgreSQL integrity violations (SQLSTATE class 23) mean the data is
	// wrong, not that the database is busy. Retrying inserts the same
	// rejected row again.
	if isConstraintViolation(err) {
		return ClassPermanent
	}

	return ClassTransient
}

func isConstraintViolation(err error) bool {
	// Matching on the text rather than importing pgconn keeps this package
	// free of a database dependency; the codes are stable and specific.
	msg := err.Error()
	for _, code := range []string{
		"23502", // not_null_violation
		"23503", // foreign_key_violation
		"23505", // unique_violation
		"23514", // check_violation
		"22P02", // invalid_text_representation
	} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}
