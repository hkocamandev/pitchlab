package httpx

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Page is the pagination block every list response carries.
type Page struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// List is the envelope for every collection endpoint.
type List[T any] struct {
	Data []T  `json:"data"`
	Page Page `json:"page"`
}

// Cursor is an opaque position in a result set.
//
// Keyset, not offset: pitches are appended continuously, so an offset shifts
// under the client and makes rows repeat or vanish between pages.
//
// The ID tiebreaker is not optional. Replay can emit several pitches with the
// same timestamp, and paginating on time alone silently skips all but one of
// them at a page boundary.
type Cursor struct {
	Time *time.Time `json:"t,omitempty"`
	Name string     `json:"n,omitempty"`
	Int  *int32     `json:"i,omitempty"`
	ID   *uuid.UUID `json:"id,omitempty"`
}

// Encode renders a cursor as an opaque token.
//
// Opaque by intent: the encoding is an implementation detail and clients must
// not construct or parse one.
func (c Cursor) Encode() string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses a cursor token.
func DecodeCursor(token string) (Cursor, error) {
	var c Cursor
	if token == "" {
		return c, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return c, ErrValidation("cursor is not a valid token",
			FieldError{Field: "cursor", Message: "malformed"})
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, ErrValidation("cursor is not a valid token",
			FieldError{Field: "cursor", Message: "malformed"})
	}
	return c, nil
}

// Limit reads and bounds the page size.
//
// Querying one extra row is how HasMore is determined without a second COUNT
// query, which on a table this size would cost as much as the page itself.
func Limit(r *http.Request, def, max int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, ErrValidation("limit must be an integer",
			FieldError{Field: "limit", Message: "not a number"})
	}
	if n < 1 || n > max {
		return 0, ErrValidation(
			"limit must be between 1 and "+strconv.Itoa(max),
			FieldError{Field: "limit", Message: "out of range"})
	}
	return n, nil
}

// Paginate trims an over-fetched slice to the page size and reports whether
// more rows exist. Call it with limit+1 rows fetched.
func Paginate[T any](rows []T, limit int) (page []T, hasMore bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// --- query parameter helpers ----------------------------------------------

// QueryUUID reads a required UUID query parameter.
func QueryUUID(r *http.Request, key string) (uuid.UUID, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return uuid.Nil, ErrValidation(key+" is required",
			FieldError{Field: key, Message: "required"})
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, ErrValidation(key+" is not a UUID",
			FieldError{Field: key, Message: "not a UUID"})
	}
	return id, nil
}

// PathUUID reads a UUID from a path segment.
func PathUUID(r *http.Request, key string) (uuid.UUID, error) {
	raw := r.PathValue(key)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, ErrValidation(key+" is not a UUID",
			FieldError{Field: key, Message: "not a UUID"})
	}
	return id, nil
}

// QueryString reads an optional string parameter.
func QueryString(r *http.Request, key string) *string {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return nil
	}
	return &v
}

// QueryEnum reads an optional parameter constrained to a fixed set.
func QueryEnum(r *http.Request, key string, allowed ...string) (*string, error) {
	v := QueryString(r, key)
	if v == nil {
		return nil, nil
	}
	for _, a := range allowed {
		if strings.EqualFold(a, *v) {
			return &a, nil
		}
	}
	return nil, ErrValidation(
		key+" must be one of: "+strings.Join(allowed, ", "),
		FieldError{Field: key, Message: "invalid value"})
}

// QueryBool reads an optional boolean parameter.
func QueryBool(r *http.Request, key string) (*bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, ErrValidation(key+" must be true or false",
			FieldError{Field: key, Message: "not a boolean"})
	}
	return &b, nil
}

// QueryFloat32 reads an optional float parameter.
func QueryFloat32(r *http.Request, key string) (*float32, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return nil, ErrValidation(key+" must be a number",
			FieldError{Field: key, Message: "not a number"})
	}
	v := float32(f)
	return &v, nil
}

// QueryTime reads an optional date or timestamp parameter, accepting either
// a plain date or a full RFC 3339 timestamp.
func QueryTime(r *http.Request, key string) (*time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			utc := t.UTC()
			return &utc, nil
		}
	}
	return nil, ErrValidation(key+" must be a date or RFC 3339 timestamp",
		FieldError{Field: key, Message: "not a timestamp"})
}

// QueryIncludes parses a comma-separated include list.
func QueryIncludes(r *http.Request, defaults ...string) map[string]bool {
	raw := strings.TrimSpace(r.URL.Query().Get("include"))
	out := map[string]bool{}
	if raw == "" {
		for _, d := range defaults {
			out[d] = true
		}
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[strings.ToLower(p)] = true
		}
	}
	return out
}
