package observability

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
)

// ClassifyMLError reduces a failure to one of a fixed set of labels.
//
// A fixed vocabulary, never the error's text. An error message can contain a
// host, a port, an identifier or a whole SQL statement, and using it as a
// label value is the same cardinality mistake as labelling by session id --
// only harder to spot, because it usually looks fine until the one day
// something includes a unique value in its message.
func ClassifyMLError(err error) string {
	if err == nil {
		return "none"
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return "timeout"
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "circuit breaker is open"):
		return "breaker_open"
	case strings.Contains(msg, "connection refused"):
		return "unreachable"
	case strings.Contains(msg, "no such host"):
		return "unreachable"
	case strings.Contains(msg, "status 4"):
		return "rejected"
	case strings.Contains(msg, "status 5"):
		return "server_error"
	}
	return "unknown"
}
