package observability

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler serves the scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A failing collector should be visible in the scrape rather than
		// turning the whole endpoint into a 500 that hides every other metric.
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// InstrumentRoute wraps one handler with the metrics for its route.
//
// Per route rather than one middleware around the whole mux, because the label
// has to be the registered pattern and not the requested path. "/api/v1/
// athletes/0198f2a1-..." would create a time series per athlete; the pattern
// it matched is one of a fixed set. Wrapping at registration is the only place
// the pattern is known for certain.
func (m *Metrics) InstrumentRoute(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.HTTPInFlight.Inc()
		defer m.HTTPInFlight.Dec()

		start := time.Now()
		rec := &statusWriter{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		status := strconv.Itoa(rec.status())
		m.HTTPDuration.WithLabelValues(r.Method, route, status).
			Observe(time.Since(start).Seconds())
		m.HTTPTotal.WithLabelValues(r.Method, route, status).Inc()
	})
}

// statusWriter records the status code.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack forwards the connection takeover a WebSocket upgrade needs.
//
// Implemented for the same reason it is implemented on the access log's
// wrapper: the upgrade library type-asserts http.Hijacker directly, and a
// wrapper that does not implement it fails every handshake with a 500. Adding
// a metrics wrapper without this would have reintroduced a bug that already
// cost a debugging session once.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("observability: response writer does not support hijacking")
	}
	w.code = http.StatusSwitchingProtocols
	return hijacker.Hijack()
}
