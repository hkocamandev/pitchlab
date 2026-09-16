package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hkocamandev/pitchlab/internal/auth"
	"github.com/hkocamandev/pitchlab/internal/httpx"
)

type claimsKey struct{}

// ClaimsFrom returns the verified identity carried on a request.
func ClaimsFrom(ctx context.Context) (auth.Claims, bool) {
	claims, ok := ctx.Value(claimsKey{}).(auth.Claims)
	return claims, ok
}

// Roles.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// TokenFromRequest extracts a token without verifying it.
//
// Two sources, in this order:
//
//  1. the session cookie, which is how the dashboard authenticates and how the
//     WebSocket handshake authenticates, because a browser attaches cookies to
//     it automatically;
//  2. an Authorization: Bearer header, for anything that is not a browser --
//     curl, a script, a test.
//
// A query parameter is deliberately not a source. The original design chose
// one for the WebSocket, on the grounds that the browser API cannot send
// custom headers; the cookie solves that better, and a token in a URL ends up
// in access logs, browser history and Referer headers.
func TokenFromRequest(r *http.Request) string {
	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

// RequireAuth rejects a request without a valid token.
func (s *Server) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := s.verifyRequest(r)
		if err != nil {
			httpx.WriteProblem(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), claimsKey{}, claims)))
	})
}

// RequireRole rejects a request whose identity lacks a role.
//
// Checked after authentication, and reported as 403 rather than 401: the
// caller proved who they are, and no different credential of theirs would
// help. Answering 401 would send the dashboard back to the login page in a
// loop.
func (s *Server) RequireRole(role string, next http.Handler) http.Handler {
	return s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFrom(r.Context())
		if !ok || claims.Role != role {
			s.log.Warn("authorisation denied",
				"user_id", claims.Subject, "have_role", claims.Role,
				"need_role", role, "path", r.URL.Path)
			httpx.WriteProblem(w, r, httpx.ErrForbidden(
				"this action requires the "+role+" role"))
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// verifyRequest authenticates a request and returns its claims.
func (s *Server) verifyRequest(r *http.Request) (auth.Claims, error) {
	if s.signer == nil {
		return auth.Claims{}, httpx.ErrUnavailable("authentication is not configured", nil)
	}

	token := TokenFromRequest(r)
	if token == "" {
		return auth.Claims{}, httpx.ErrUnauthorized("no session")
	}

	claims, err := s.signer.Verify(token)
	if err != nil {
		// One message for every failure. Which check failed -- signature,
		// expiry, issuer -- is useful only to somebody probing the
		// implementation. The reason is logged, not returned.
		s.log.Debug("token rejected", "error", err, "path", r.URL.Path)

		if errors.Is(err, auth.ErrTokenExpired) {
			// The one distinction worth making, because the client's correct
			// response differs: an expired session means log in again, while
			// a bad signature means something is wrong.
			return claims, httpx.ErrUnauthorized("session expired")
		}
		return claims, httpx.ErrUnauthorized("invalid session")
	}

	return claims, nil
}

// RequireSameOrigin blocks cross-site state-changing requests.
//
// SameSite=Strict already stops a browser from attaching the session cookie to
// a cross-site request, so this is the second layer rather than the first. It
// exists because SameSite is enforced by the browser and this check is
// enforced by the server, and a defence that depends only on the client is a
// defence that assumes the client is honest.
//
// Applied to state-changing methods only: a GET carries no side effects here,
// and requiring an Origin on reads would break ordinary tooling.
func (s *Server) RequireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin != "" && !s.cfg.AllowsOrigin(origin) {
			s.log.Warn("cross-origin write refused",
				"origin", origin, "path", r.URL.Path, "method", r.Method)
			httpx.WriteProblem(w, r, httpx.ErrForbidden("origin is not allowed"))
			return
		}

		next.ServeHTTP(w, r)
	})
}
