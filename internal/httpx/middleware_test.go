package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if seen == "" {
		t.Fatal("no request id in context")
	}
	if got := rec.Header().Get(HeaderRequestID); got != seen {
		t.Fatalf("header %q does not match context %q", got, seen)
	}
}

func TestRequestIDHonoursAClientSuppliedValue(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderRequestID, "client-supplied-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "client-supplied-123" {
		t.Fatalf("got %q, want the client value", seen)
	}
}

func TestRequestIDRejectsUnsafeClientValues(t *testing.T) {
	// The id goes into structured logs verbatim. An unbounded or
	// punctuation-carrying header would let a caller inject into them.
	cases := []string{
		"has spaces",
		`{"json":"injection"}`,
		"line\nbreak",
		strings.Repeat("x", 65),
	}

	for _, bad := range cases {
		var seen string
		h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestIDFrom(r.Context())
		}))
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(HeaderRequestID, bad)
		h.ServeHTTP(httptest.NewRecorder(), req)

		if seen == bad {
			t.Errorf("unsafe request id was accepted: %q", bad)
		}
		if seen == "" {
			t.Errorf("a rejected id should still be replaced by a generated one")
		}
	}
}

func TestCorrelationIDIsCarriedSeparately(t *testing.T) {
	// Request id and correlation id are different lifetimes: one HTTP
	// request versus one pitch's journey across every service. Merging them
	// would lose the chain.
	var reqID, corrID string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID = RequestIDFrom(r.Context())
		corrID = CorrelationIDFrom(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderCorrelationID, "01JX7KABCDEF")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if corrID != "01JX7KABCDEF" {
		t.Fatalf("correlation id lost: %q", corrID)
	}
	if reqID == corrID {
		t.Fatal("request id and correlation id must not collapse into one value")
	}
	if rec.Header().Get(HeaderCorrelationID) != "01JX7KABCDEF" {
		t.Fatal("correlation id should be echoed so a browser can see the chain")
	}
}

func TestRecoveryTurnsAPanicIntoAProblemResponse(t *testing.T) {
	// A panic in one handler must not take the process down and every other
	// in-flight request with it.
	h := RequestID(Recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/athletes", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", rec.Code)
	}

	var problem Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not a problem document: %v", err)
	}
	if problem.RequestID == "" {
		t.Error("a recovered panic must still be traceable to a request id")
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic detail leaked to the client")
	}
}

func TestCORSRejectsUnknownOrigins(t *testing.T) {
	// The same allowlist gates the WebSocket handshake, and WebSocket is not
	// subject to the same-origin policy: an unchecked Origin lets any page
	// open a socket.
	allowed := func(o string) bool { return o == "http://localhost:5173" }
	h := CORS(allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("an unknown origin must not be granted access")
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Fatal("a known origin should be echoed back")
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Error("Vary: Origin is required or a shared cache will serve the wrong headers")
	}
}

func TestCORSPreflightFromAnUnknownOriginIsForbidden(t *testing.T) {
	allowed := func(o string) bool { return o == "http://localhost:5173" }
	h := CORS(allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("preflight should not reach the handler")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/athletes", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

func TestWriteProblemHidesInternalDetail(t *testing.T) {
	h := RequestID(Logging(slog.Default())(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			WriteProblem(w, r, ErrInternal(
				errPlain("pq: relation \"secret_table\" does not exist")))
		})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret_table") {
		t.Fatal("internal error text reached the client; it belongs in the logs")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type %q, want application/problem+json", ct)
	}
}

func TestWriteProblemPreservesClientErrorDetail(t *testing.T) {
	// Client errors are the caller's to fix, so they carry the specifics.
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteProblem(w, r, ErrValidation("limit must be between 1 and 500",
			FieldError{Field: "limit", Message: "out of range"}))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/x?limit=9999", nil))

	var problem Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || problem.Status != http.StatusBadRequest {
		t.Fatalf("got %d / %d, want 400", rec.Code, problem.Status)
	}
	if len(problem.Errors) != 1 || problem.Errors[0].Field != "limit" {
		t.Fatalf("field errors missing: %+v", problem.Errors)
	}
	if problem.Instance != "/api/v1/x" {
		t.Errorf("instance %q should name the failing path", problem.Instance)
	}
	if problem.RequestID == "" {
		t.Error("request id missing; a user report could not be traced")
	}
}

type errPlain string

func (e errPlain) Error() string { return string(e) }
