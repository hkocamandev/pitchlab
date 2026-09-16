package httpx

import (
	"net/http"
	"strings"
)

// MaxRequestBody bounds how much a handler will read.
//
// Sixty-four kilobytes is far more than any request this API accepts -- the
// largest is a login with two short strings -- and far less than what an
// unbounded read costs when somebody points a generator at it. The WebSocket
// side has had the same kind of cap since it was written.
const MaxRequestBody = 64 << 10

// LimitBody caps the request body.
//
// MaxBytesReader rather than io.LimitReader: the former makes the read fail
// with an error the handler can report, while the latter silently truncates
// and hands the decoder a half message, which is the worse failure.
func LimitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets the headers a browser needs to be told explicitly.
//
// These are cheap and they only matter when something else has already gone
// wrong -- which is precisely the argument for having them. None of them
// prevents a bug; each one limits what a bug can be turned into.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Stops a browser from guessing that a JSON response is really HTML
		// and running it. The API returns JSON and problem documents only, so
		// there is never a reason to sniff.
		h.Set("X-Content-Type-Options", "nosniff")

		// Nothing here is meant to be embedded. Framing it is only useful to
		// somebody building a clickjacking overlay.
		h.Set("X-Frame-Options", "DENY")

		// Identifiers appear in paths. Without this, following a link out of
		// the dashboard would hand the destination a URL containing a session
		// or athlete id.
		h.Set("Referrer-Policy", "no-referrer")

		// This API is not a browser-features consumer; switching them off is
		// free.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

		// A JSON API has no scripts, styles or frames of its own. If a
		// response somehow renders as a document, this policy means it can do
		// nothing.
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

		// Only meaningful over TLS, and actively harmful over plain HTTP in
		// development -- a browser that caches it will refuse to load the
		// local site over http afterwards. Set only when a proxy reports the
		// request arrived over https.
		if forwardedProto(r) == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// forwardedProto reports the scheme the client actually used.
//
// TLS terminates at a proxy in every realistic deployment, so the application
// cannot learn this from its own listener.
func forwardedProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	// X-Forwarded-Proto is trusted here because this service is only ever
	// reached through a proxy it controls. Exposed directly to the internet
	// the header would be attacker-supplied, and the correct handling would be
	// to ignore it unless the peer is a known proxy.
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return strings.ToLower(strings.TrimSpace(strings.Split(proto, ",")[0]))
	}
	return "http"
}

// IsSecureRequest reports whether the client reached this service over TLS.
func IsSecureRequest(r *http.Request) bool { return forwardedProto(r) == "https" }

// ClientIP returns the address a request appears to come from.
//
// Used only for throttling, never for authorisation. Behind a proxy the header
// is the only usable value; exposed directly it is attacker-controlled, which
// is survivable for a rate-limit key -- the worst case is that an attacker
// throttles themselves under many keys, and the per-username limit still
// applies.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return strings.TrimSpace(real)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
