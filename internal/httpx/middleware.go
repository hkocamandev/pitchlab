package httpx

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxCorrelationID
	ctxLogger
)

// Header names. Request ID and correlation ID are different things and are
// deliberately not merged:
//
//	X-Request-ID      one HTTP request. Milliseconds. Born at the edge.
//	X-Correlation-ID  one pitch's journey across every service and topic.
//	                  Seconds. Born in the replay simulator, carried through
//	                  the Kafka envelope, and echoed back out to the browser.
const (
	HeaderRequestID     = "X-Request-ID"
	HeaderCorrelationID = "X-Correlation-ID"

	// HeaderCacheState reports whether a response came from the cache. It
	// makes a cache problem visible from outside the process, rather than only
	// in a metric nobody happens to be looking at.
	HeaderCacheState = "X-Cache"
)

// RequestID assigns an identifier to every request and echoes it back.
//
// A client-supplied value is honoured so a caller can correlate across its
// own logs, but only if it looks like an identifier: an unbounded header
// would otherwise end up in structured logs verbatim.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeID(r.Header.Get(HeaderRequestID))
		if id == "" {
			id = uuid.NewString()
		}

		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		if corr := sanitizeID(r.Header.Get(HeaderCorrelationID)); corr != "" {
			ctx = context.WithValue(ctx, ctxCorrelationID, corr)
			w.Header().Set(HeaderCorrelationID, corr)
		}

		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, c := range s {
		isAllowed := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_'
		if !isAllowed {
			return ""
		}
	}
	return s
}

// RequestIDFrom returns the request id, if any.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// CorrelationIDFrom returns the correlation id, if any.
func CorrelationIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxCorrelationID).(string)
	return id
}

// LoggerFrom returns the request-scoped logger, falling back to the default.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(ctxLogger).(*slog.Logger); ok {
		return log
	}
	return slog.Default()
}

// WithLogger attaches a logger to a context.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxLogger, log)
}

// statusRecorder captures what was actually sent, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack takes the connection over, which is how a WebSocket upgrade works.
//
// Implementing this explicitly is not optional. Unwrap alone only helps
// callers that go through http.ResponseController; a library that type-asserts
// http.Hijacker directly -- which the WebSocket upgrader does -- sees a
// wrapper that does not implement it and fails the handshake with a 500. That
// is exactly how this was found: every upgrade behind the access log returned
// "Internal Server Error" while the same handler worked without it.
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("httpx: %T does not support hijacking",
			w.ResponseWriter)
	}
	// Nothing further will be written through this recorder, so the status is
	// recorded here or the access log reports a 200 for every upgrade.
	w.status = http.StatusSwitchingProtocols
	return hijacker.Hijack()
}

// Logging attaches a request-scoped logger and writes one access log line per
// request.
func Logging(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			log := base.With("request_id", RequestIDFrom(r.Context()))
			if corr := CorrelationIDFrom(r.Context()); corr != "" {
				log = log.With("correlation_id", corr)
			}

			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r.WithContext(WithLogger(r.Context(), log)))

			if rec.status == 0 {
				rec.status = http.StatusOK
			}

			// Health and metrics scrapes would otherwise dominate the log.
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				return
			}

			log.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
				"remote", r.RemoteAddr,
			)
		})
	}
}

// Recovery turns a panic into a 500 instead of a dropped connection.
//
// A panic in one handler must not take down the process and every other
// in-flight request with it.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A hijacked connection (WebSocket) has no response left to write.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			LoggerFrom(r.Context()).Error("panic recovered",
				"panic", rec,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			WriteProblem(w, r, ErrInternal(nil))
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS applies an explicit origin allowlist.
//
// There is no wildcard. Config rejects "*" at startup, because the same
// allowlist gates the WebSocket handshake, and WebSocket is not subject to
// the same-origin policy: an unchecked Origin lets any page open a socket.
func CORS(allowed func(string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if origin != "" && allowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Expose-Headers",
					strings.Join([]string{
						HeaderRequestID, HeaderCorrelationID, HeaderCacheState, "ETag",
					}, ", "))
			}

			if r.Method == http.MethodOptions {
				if origin != "" && !allowed(origin) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					strings.Join([]string{"Content-Type", HeaderRequestID, HeaderCorrelationID, "If-None-Match"}, ", "))
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler execution.
//
// It cancels the request context, which propagates into database calls, so a
// slow query is abandoned rather than held until the client gives up.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
