package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
	"github.com/hkocamandev/pitchlab/internal/httpx"
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

// Routes builds the HTTP handler.
//
// net/http's own mux handles method-and-pattern routing since Go 1.22, so a
// third-party router would add a dependency for syntax alone.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints sit outside /api/v1: they are infrastructure, not
	// product surface, and must not move when the API is versioned.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// athletes
	mux.Handle("GET /api/v1/athletes", s.handler(s.listAthletes))
	mux.Handle("GET /api/v1/athletes/{athleteId}", s.handler(s.getAthlete))
	mux.Handle("GET /api/v1/athletes/{athleteId}/analytics", s.handler(s.getAthleteAnalytics))
	mux.Handle("GET /api/v1/athletes/{athleteId}/sessions", s.handler(s.listAthleteSessions))
	mux.Handle("GET /api/v1/athletes/{athleteId}/anomalies", s.handler(s.listAthleteAnomalies))

	// sessions
	mux.Handle("GET /api/v1/sessions", s.handler(s.listSessions))
	mux.Handle("GET /api/v1/sessions/{sessionId}", s.handler(s.getSession))
	mux.Handle("GET /api/v1/sessions/{sessionId}/pitches", s.handler(s.listSessionPitches))
	mux.Handle("POST /api/v1/sessions/{sessionId}/close", s.handler(s.closeSession))

	// pitches and predictions
	mux.Handle("GET /api/v1/pitches", s.handler(s.listPitches))
	mux.Handle("GET /api/v1/pitches/{pitchId}", s.handler(s.getPitch))
	mux.Handle("GET /api/v1/pitches/{pitchId}/predictions", s.handler(s.listPitchPredictions))

	// models
	mux.Handle("GET /api/v1/models", s.handler(s.listModels))

	// anomalies
	mux.Handle("POST /api/v1/anomalies/{anomalyId}/acknowledge", s.handler(s.acknowledgeAnomaly))

	// The real-time channel sits outside /api/v1. It is a different protocol
	// with a different lifecycle, and versioning it alongside REST resources
	// would imply they change together.
	if s.wsHandler != nil {
		mux.Handle("GET /ws/sessions/{sessionId}", s.wsHandler)
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
