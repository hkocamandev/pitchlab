package config

import (
	"strings"
	"testing"
	"time"
)

func valid() API {
	return API{
		Addr:            ":8081",
		DatabaseURL:     "postgres://u:p@localhost:5432/db?sslmode=disable",
		RedisURL:        "redis://localhost:6379/0",
		AllowedOrigins:  []string{"http://localhost:5173"},
		DefaultPageSize: 50,
		MaxPageSize:     500,
		LogLevel:        "info",
		LogFormat:       "json",
		ReadTimeout:     30 * time.Second,
	}
}

func TestValidConfigPasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("expected valid: %v", err)
	}
}

func TestWildcardOriginIsRejected(t *testing.T) {
	// The same allowlist gates the WebSocket handshake. WebSocket ignores the
	// same-origin policy, so a wildcard here lets any site open a socket --
	// this must not be reachable by configuration accident.
	cfg := valid()
	cfg.AllowedOrigins = []string{"*"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a wildcard origin must be rejected")
	}
	if !strings.Contains(err.Error(), "origin checking") {
		t.Fatalf("the error should explain why: %v", err)
	}
}

func TestEmptyOriginListIsRejected(t *testing.T) {
	cfg := valid()
	cfg.AllowedOrigins = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("an empty allowlist must be rejected rather than silently permissive")
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	// One problem per restart is a miserable way to configure a service.
	cfg := valid()
	cfg.Addr = ""
	cfg.DatabaseURL = ""
	cfg.LogLevel = "verbose"
	cfg.DefaultPageSize = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"PITCHLAB_API_ADDR", "DATABASE_URL", "PITCHLAB_LOG_LEVEL", "PITCHLAB_DEFAULT_PAGE_SIZE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestDefaultPageSizeCannotExceedMax(t *testing.T) {
	cfg := valid()
	cfg.DefaultPageSize = 1000
	cfg.MaxPageSize = 500
	if err := cfg.Validate(); err == nil {
		t.Fatal("a default above the maximum is incoherent and must fail")
	}
}

func TestAllowsOrigin(t *testing.T) {
	cfg := valid()
	cfg.AllowedOrigins = []string{"http://localhost:5173", "https://app.example"}

	cases := map[string]bool{
		"http://localhost:5173": true,
		"HTTP://LOCALHOST:5173": true, // origins are case-insensitive
		"https://app.example":   true,
		"https://evil.example":  false,
		"http://localhost:5174": false,
		// A non-browser client sends no Origin; CORS does not apply to it.
		"": true,
	}
	for origin, want := range cases {
		if got := cfg.AllowsOrigin(origin); got != want {
			t.Errorf("AllowsOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestUnparseableDurationFailsRatherThanFallingBack(t *testing.T) {
	// Silently substituting a default would hide the typo, and the operator
	// would never learn that their timeout was ignored.
	t.Setenv("PITCHLAB_READ_TIMEOUT", "30 seconds")
	t.Setenv("PITCHLAB_ALLOWED_ORIGINS", "http://localhost:5173")

	if got := duration("PITCHLAB_READ_TIMEOUT", 30*time.Second); got != 0 {
		t.Fatalf("got %v, want zero so validation rejects it", got)
	}
}
