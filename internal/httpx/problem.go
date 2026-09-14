// Package httpx holds the HTTP plumbing shared by every handler: the error
// format, request identity, logging, and pagination.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Problem is an RFC 7807 "Problem Details" response body.
//
// One error shape everywhere means a client writes one error path, and every
// failure carries a RequestID so a user report can be traced to a log line.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Instance  string       `json:"instance,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`
}

// FieldError locates a validation failure in the request.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

const problemBase = "https://pitchlab.dev/errors/"

func (p Problem) Error() string { return p.Title + ": " + p.Detail }

// APIError is an error that knows how it should be rendered.
type APIError struct {
	Status int
	Slug   string
	Title  string
	Detail string
	Fields []FieldError
	// Err is the underlying cause. It is logged, never sent to the client:
	// internal errors leak schema and topology.
	Err error
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return e.Title + ": " + e.Err.Error()
	}
	return e.Title + ": " + e.Detail
}

func (e *APIError) Unwrap() error { return e.Err }

// Constructors for the failures handlers actually produce.

func ErrNotFound(resource string) *APIError {
	return &APIError{
		Status: http.StatusNotFound, Slug: "not-found",
		Title: "Resource not found", Detail: resource + " does not exist",
	}
}

func ErrValidation(detail string, fields ...FieldError) *APIError {
	return &APIError{
		Status: http.StatusBadRequest, Slug: "validation-failed",
		Title: "Validation failed", Detail: detail, Fields: fields,
	}
}

func ErrConflict(detail string) *APIError {
	return &APIError{
		Status: http.StatusConflict, Slug: "conflict",
		Title: "Conflict", Detail: detail,
	}
}

// ErrForbidden is for a request that is well-formed and understood but not
// allowed. The WebSocket handshake uses it for a rejected Origin: the browser
// asked correctly, from a site that may not connect.
func ErrForbidden(detail string) *APIError {
	return &APIError{
		Status: http.StatusForbidden, Slug: "forbidden",
		Title: "Forbidden", Detail: detail,
	}
}

func ErrUnprocessable(detail string) *APIError {
	return &APIError{
		Status: http.StatusUnprocessableEntity, Slug: "unprocessable",
		Title: "Unprocessable request", Detail: detail,
	}
}

func ErrInternal(err error) *APIError {
	return &APIError{
		Status: http.StatusInternalServerError, Slug: "internal",
		Title: "Internal server error",
		// Deliberately generic: the cause goes to the logs under the same
		// request id, not to the caller.
		Detail: "an unexpected error occurred", Err: err,
	}
}

func ErrUnavailable(detail string, err error) *APIError {
	return &APIError{
		Status: http.StatusServiceUnavailable, Slug: "unavailable",
		Title: "Service unavailable", Detail: detail, Err: err,
	}
}

// WriteProblem renders err as a problem document and logs it.
//
// Anything that is not an APIError becomes a 500 with a generic body, so an
// unexpected error cannot accidentally expose internals.
func WriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = ErrInternal(err)
	}

	reqID := RequestIDFrom(r.Context())
	problem := Problem{
		Type:      problemBase + apiErr.Slug,
		Title:     apiErr.Title,
		Status:    apiErr.Status,
		Detail:    apiErr.Detail,
		Instance:  r.URL.Path,
		RequestID: reqID,
		Errors:    apiErr.Fields,
	}

	log := LoggerFrom(r.Context())
	attrs := []any{
		"status", apiErr.Status,
		"path", r.URL.Path,
		"method", r.Method,
	}
	if apiErr.Err != nil {
		attrs = append(attrs, "error", apiErr.Err.Error())
	}
	if apiErr.Status >= 500 {
		log.Error(apiErr.Title, attrs...)
	} else {
		log.Warn(apiErr.Title, attrs...)
	}

	WriteJSON(w, r, apiErr.Status, problem)
}

// WriteJSON writes a JSON response.
//
// The body is encoded before any header is written: encoding a value can
// fail, and a failure after WriteHeader leaves the client with a truncated
// body under a success status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		LoggerFrom(r.Context()).Error("encode response", "error", err)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"` + problemBase + `internal","title":"Internal server error","status":500}`))
		return
	}

	contentType := "application/json"
	if status >= 400 {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(body); err != nil {
		// The client hung up mid-write. Nothing to do but record it.
		LoggerFrom(r.Context()).Debug("write response", "error", err)
	}
}

// WriteNoContent writes a 204.
func WriteNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

var _ = slog.LevelInfo // keep slog imported for the logger helpers
