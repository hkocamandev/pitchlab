package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hkocamandev/pitchlab/internal/cache"
	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/httpx"
	"github.com/hkocamandev/pitchlab/internal/mlclient"
	"github.com/hkocamandev/pitchlab/internal/observability"
)

// Server holds the handler dependencies.
type Server struct {
	cfg   config.API
	store *db.Store
	log   *slog.Logger
	// redis is optional by design. Its absence degrades performance, never
	// correctness, so it is allowed to be nil.
	redis RedisChecker
	// wsHandler is optional too: with no hub attached the REST surface still
	// serves, and the route simply does not exist.
	wsHandler http.Handler
	// cache is optional and may be nil. Every method on it tolerates that, so
	// there is no "is the cache configured" branch in any handler, and no path
	// by which a Redis problem becomes a failed request.
	cache *cache.Cache
	// ml is optional too, and only the explanation endpoint uses it. Scores
	// are computed by the stream processor and read from the database; this
	// API never scores a pitch on the request path.
	ml *mlclient.Client
	// metrics is optional. With none attached the routes are registered
	// unwrapped and /metrics is absent, which is what the handler tests use.
	metrics *observability.Metrics
}

// RedisChecker is the slice of Redis the API needs for readiness. Keeping it
// an interface means the cache can be added in a later phase without this
// package changing.
type RedisChecker interface {
	Ping(ctx context.Context) error
}

// New builds a server.
func New(cfg config.API, store *db.Store, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: store, log: log}
}

// WithRedis attaches a cache for readiness reporting.
func (s *Server) WithRedis(r RedisChecker) *Server {
	s.redis = r
	return s
}

// WithInference attaches the inference client used for on-demand
// explanations.
//
// Deliberately not used for scoring. A prediction belongs to the pitch and is
// written once, by the processor, with the model version that produced it;
// scoring again on a read would produce a number that disagrees with the
// stored one as soon as the model is retrained.
func (s *Server) WithInference(ml *mlclient.Client) *Server {
	s.ml = ml
	return s
}

// WithCache attaches the Redis layer and reports it in readiness.
//
// Readiness reports it as degraded, never as unready: taking the whole API out
// of rotation over a cache outage is the failure this design exists to avoid.
func (s *Server) WithCache(c *cache.Cache) *Server {
	s.cache = c
	if c != nil {
		s.redis = c
	}
	return s
}

// WithMetrics attaches the Prometheus registry and exposes /metrics.
func (s *Server) WithMetrics(m *observability.Metrics) *Server {
	s.metrics = m
	return s
}

// route registers one endpoint, wrapped in its own instrumentation.
//
// The pattern is passed rather than read from the request, because the metric
// label has to be the pattern and not the path: "/api/v1/athletes/{athleteId}"
// is one time series, while the paths it matches are one per athlete. A single
// middleware around the mux cannot know which pattern matched, so the wrapping
// happens where the pattern is written down.
func (s *Server) route(mux *http.ServeMux, pattern string, h http.Handler) {
	if s.metrics != nil {
		h = s.metrics.InstrumentRoute(routeLabel(pattern), h)
	}
	mux.Handle(pattern, h)
}

// routeLabel drops the method from a pattern, which carries it as its own
// label already.
func routeLabel(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// Routes builds the HTTP handler.
//
// net/http's own mux handles method-and-pattern routing since Go 1.22, so a
// third-party router would add a dependency for syntax alone.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints sit outside /api/v1: they are infrastructure, not
	// product surface, and must not move when the API is versioned.
	s.route(mux, "GET /healthz", http.HandlerFunc(s.handleHealthz))
	s.route(mux, "GET /readyz", http.HandlerFunc(s.handleReadyz))

	// The scrape endpoint is registered only when metrics are attached, so a
	// server built without them does not advertise an endpoint that would
	// answer with nothing.
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics.Handler())
	}

	// athletes
	s.route(mux, "GET /api/v1/athletes", s.handler(s.listAthletes))
	s.route(mux, "GET /api/v1/athletes/{athleteId}", s.handler(s.getAthlete))
	s.route(mux, "GET /api/v1/athletes/{athleteId}/analytics", s.handler(s.getAthleteAnalytics))
	s.route(mux, "GET /api/v1/athletes/{athleteId}/sessions", s.handler(s.listAthleteSessions))
	s.route(mux, "GET /api/v1/athletes/{athleteId}/anomalies", s.handler(s.listAthleteAnomalies))

	// sessions
	s.route(mux, "GET /api/v1/sessions", s.handler(s.listSessions))
	s.route(mux, "GET /api/v1/sessions/{sessionId}", s.handler(s.getSession))
	s.route(mux, "GET /api/v1/sessions/{sessionId}/pitches", s.handler(s.listSessionPitches))
	s.route(mux, "POST /api/v1/sessions/{sessionId}/close", s.handler(s.closeSession))

	// pitches and predictions
	s.route(mux, "GET /api/v1/pitches/{pitchId}/explanation", s.handler(s.getPitchExplanation))
	s.route(mux, "GET /api/v1/pitches", s.handler(s.listPitches))
	s.route(mux, "GET /api/v1/pitches/{pitchId}", s.handler(s.getPitch))
	s.route(mux, "GET /api/v1/pitches/{pitchId}/predictions", s.handler(s.listPitchPredictions))

	// models
	s.route(mux, "GET /api/v1/models", s.handler(s.listModels))

	// anomalies
	s.route(mux, "POST /api/v1/anomalies/{anomalyId}/acknowledge", s.handler(s.acknowledgeAnomaly))

	// The real-time channel sits outside /api/v1. It is a different protocol
	// with a different lifecycle, and versioning it alongside REST resources
	// would imply they change together.
	if s.wsHandler != nil {
		s.route(mux, "GET /ws/sessions/{sessionId}", s.wsHandler)
	}

	// Order matters: RequestID runs first so everything downstream, including
	// the panic handler, can label its output with the request id.
	var h http.Handler = mux
	// Timeout only bounds the handler's own execution -- for a WebSocket that
	// is the handshake, since the pumps outlive the request and hold the
	// hijacked connection, not the request context.
	h = httpx.Timeout(s.cfg.ReadTimeout)(h)
	h = httpx.Recovery(h)
	h = httpx.CORS(s.cfg.AllowsOrigin)(h)
	h = httpx.Logging(s.log)(h)
	h = httpx.RequestID(h)
	return h
}

// handlerFunc is a handler that may fail. Returning an error rather than
// writing one means no handler can forget to set a status code.
type handlerFunc func(http.ResponseWriter, *http.Request) error

func (s *Server) handler(fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteProblem(w, r, err)
		}
	})
}

// HTTPServer builds the configured http.Server.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// --- health ---------------------------------------------------------------

type checkResult struct {
	Status    string  `json:"status"`
	LatencyMs float64 `json:"latency_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type readyResponse struct {
	Status   string                 `json:"status"`
	Version  string                 `json:"version"`
	Checks   map[string]checkResult `json:"checks"`
	Degraded []string               `json:"degraded,omitempty"`
}

// handleHealthz is liveness. It checks nothing.
//
// If liveness probed the database, one slow query would mark every instance
// unhealthy, the orchestrator would restart all of them at once, and a blip
// would become an outage. Liveness answers "is this process wedged"; that is
// all it should answer.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, map[string]string{
		"status": "ok", "version": s.cfg.Version,
	})
}

// handleReadyz is readiness: can this instance serve traffic.
//
// PostgreSQL is required. Redis is not: it is a cache and an ephemeral state
// store, never the record of truth, so its absence degrades performance and
// the instance stays in rotation. Reporting it as unready would take the
// whole API offline over a cache outage.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	checks := map[string]checkResult{}
	var degraded []string
	status := http.StatusOK

	start := time.Now()
	if err := s.store.Healthy(ctx); err != nil {
		checks["postgres"] = checkResult{Status: "down", Error: err.Error()}
		status = http.StatusServiceUnavailable
	} else {
		checks["postgres"] = checkResult{
			Status: "ok", LatencyMs: msSince(start),
		}
	}

	if s.redis != nil {
		start = time.Now()
		if err := s.redis.Ping(ctx); err != nil {
			checks["redis"] = checkResult{Status: "degraded", Error: err.Error()}
			degraded = append(degraded, "redis")
		} else {
			checks["redis"] = checkResult{Status: "ok", LatencyMs: msSince(start)}
		}
	}

	body := readyResponse{
		Status: "ok", Version: s.cfg.Version, Checks: checks, Degraded: degraded,
	}
	if status != http.StatusOK {
		body.Status = "unavailable"
	} else if len(degraded) > 0 {
		body.Status = "degraded"
	}

	httpx.WriteJSON(w, r, status, body)
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
