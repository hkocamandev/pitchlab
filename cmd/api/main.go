// Command api serves the REST surface under /api/v1.
//
// It is one of two binaries built from the same codebase. The API and the
// stream processor share the domain model, the repository layer and the
// validation rules -- splitting the code would mean keeping two copies of the
// same types in sync for nothing. They are split at runtime because their
// scaling profiles genuinely differ: this process is latency-bound and driven
// by unpredictable user traffic, while the processor is throughput-bound and
// driven by Kafka lag. A burst replay that saturates the processor must not
// slow the dashboard down with it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hkocamandev/pitchlab/internal/api"
	"github.com/hkocamandev/pitchlab/internal/config"
	"github.com/hkocamandev/pitchlab/internal/db"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI()
	if err != nil {
		// Configuration problems are fatal at startup by design. A process
		// that boots with half a configuration fails later, somewhere less
		// obvious, and usually under load.
		return err
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	log.Info("starting",
		"service", "pitchlab-api",
		"version", cfg.Version,
		"addr", cfg.Addr,
		"allowed_origins", cfg.AllowedOrigins,
	)

	// Signal handling is installed before anything is opened, so a Ctrl-C
	// during startup still unwinds cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, db.DefaultPoolConfig(cfg.DatabaseURL))
	if err != nil {
		return err
	}
	defer pool.Close()
	store := db.NewStore(pool)

	srv := api.New(cfg, store, log).HTTPServer()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish, then
	// close. Killing the listener outright would fail requests that were
	// moments from succeeding.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out; forcing close", "error", err)
		return srv.Close()
	}

	log.Info("stopped")
	return nil
}

func newLogger(cfg config.API) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// RFC 3339 with nanoseconds, so log lines from different services
			// interleave correctly when traced by correlation id.
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					return slog.String("ts", t.UTC().Format(time.RFC3339Nano))
				}
			}
			return a
		},
	}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	// Every line carries the service and version, so a multi-service log
	// stream stays attributable.
	return slog.New(handler).With(
		"service", "pitchlab-api",
		"version", cfg.Version,
	)
}
