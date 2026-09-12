// Package config loads and validates process configuration from the
// environment.
//
// Everything is validated at startup and a bad value is fatal. A process that
// starts with half a configuration fails later, somewhere less obvious, and
// usually under load.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// API is the configuration for the HTTP server process.
type API struct {
	Addr string

	DatabaseURL string
	RedisURL    string

	// AllowedOrigins gates both CORS and the WebSocket handshake. WebSocket
	// is not subject to the same-origin policy, so an unchecked Origin lets
	// any site open a connection. Authentication is out of scope for v1;
	// origin checking is not.
	AllowedOrigins []string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	// DefaultPageSize and MaxPageSize bound list endpoints. An unbounded
	// limit is a denial-of-service vector and an accidental table scan.
	DefaultPageSize int
	MaxPageSize     int

	LogLevel  string
	LogFormat string

	Version string
}

// LoadAPI reads the API configuration, applying defaults and validating.
func LoadAPI() (API, error) {
	cfg := API{
		Addr:              env("PITCHLAB_API_ADDR", ":8081"),
		DatabaseURL:       env("DATABASE_URL", "postgres://pitchlab:pitchlab@localhost:5432/pitchlab?sslmode=disable"),
		RedisURL:          env("REDIS_URL", "redis://localhost:6379/0"),
		AllowedOrigins:    splitCSV(env("PITCHLAB_ALLOWED_ORIGINS", "http://localhost:5173,http://localhost:3000")),
		ReadHeaderTimeout: duration("PITCHLAB_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:       duration("PITCHLAB_READ_TIMEOUT", 30*time.Second),
		// Long enough for a WebSocket upgrade to survive; the socket takes
		// over its own deadlines after the handshake.
		WriteTimeout:    duration("PITCHLAB_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:     duration("PITCHLAB_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout: duration("PITCHLAB_SHUTDOWN_TIMEOUT", 15*time.Second),
		DefaultPageSize: integer("PITCHLAB_DEFAULT_PAGE_SIZE", 50),
		MaxPageSize:     integer("PITCHLAB_MAX_PAGE_SIZE", 500),
		LogLevel:        env("PITCHLAB_LOG_LEVEL", "info"),
		LogFormat:       env("PITCHLAB_LOG_FORMAT", "json"),
		Version:         env("PITCHLAB_VERSION", "dev"),
	}

	return cfg, cfg.Validate()
}

// Validate reports every problem at once rather than one per restart.
func (c API) Validate() error {
	var problems []string

	if c.Addr == "" {
		problems = append(problems, "PITCHLAB_API_ADDR is empty")
	}
	if c.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is empty")
	} else if _, err := url.Parse(c.DatabaseURL); err != nil {
		problems = append(problems, fmt.Sprintf("DATABASE_URL is not a URL: %v", err))
	}
	if c.RedisURL != "" {
		if _, err := url.Parse(c.RedisURL); err != nil {
			problems = append(problems, fmt.Sprintf("REDIS_URL is not a URL: %v", err))
		}
	}
	if len(c.AllowedOrigins) == 0 {
		problems = append(problems,
			"PITCHLAB_ALLOWED_ORIGINS is empty; set it explicitly, "+
				"an unchecked Origin lets any site open a WebSocket")
	}
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			problems = append(problems,
				"PITCHLAB_ALLOWED_ORIGINS contains '*', which disables origin checking")
		}
	}
	if c.DefaultPageSize <= 0 {
		problems = append(problems, "PITCHLAB_DEFAULT_PAGE_SIZE must be positive")
	}
	if c.MaxPageSize <= 0 {
		problems = append(problems, "PITCHLAB_MAX_PAGE_SIZE must be positive")
	}
	if c.DefaultPageSize > c.MaxPageSize {
		problems = append(problems, "PITCHLAB_DEFAULT_PAGE_SIZE exceeds PITCHLAB_MAX_PAGE_SIZE")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, "PITCHLAB_LOG_LEVEL must be debug, info, warn or error")
	}
	switch strings.ToLower(c.LogFormat) {
	case "json", "text":
	default:
		problems = append(problems, "PITCHLAB_LOG_FORMAT must be json or text")
	}

	if len(problems) > 0 {
		return errors.New("invalid configuration:\n  - " + strings.Join(problems, "\n  - "))
	}
	return nil
}

// AllowsOrigin reports whether an Origin header is permitted.
func (c API) AllowsOrigin(origin string) bool {
	if origin == "" {
		// Not a browser request; CORS does not apply.
		return true
	}
	for _, allowed := range c.AllowedOrigins {
		if strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Returning the fallback would hide the typo. Zero fails validation.
		return 0
	}
	return d
}

func integer(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
