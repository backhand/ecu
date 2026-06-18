package controlplane

import (
	"context"
	"net/http"
	"strings"
)

// ctxKey is an unexported type for context keys defined in this package, so
// values stored under it cannot collide with keys from other packages.
type ctxKey int

// accountKey is the context key under which the authenticated account is
// stored.
const accountKey ctxKey = iota

// accountFromContext returns the authenticated account attached by
// authMiddleware. The bool is false if no account is present (which should not
// happen for handlers behind the middleware).
func accountFromContext(ctx context.Context) (string, bool) {
	acct, ok := ctx.Value(accountKey).(string)
	return acct, ok
}

// authMiddleware validates the Authorization: Bearer <key> header against the
// store and, on success, attaches the resolved account to the request context.
// Every failure mode — missing header, malformed header, empty key, unknown
// key, and disabled key — is rejected with 401 and a JSON {"detail": ...} body.
// It wraps the entire mux so all routes are authenticated.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}
		account, active, found, err := s.store.LookupKey(key)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !found || !active {
			// Unknown and disabled keys are indistinguishable to the client.
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		ctx := context.WithValue(r.Context(), accountKey, account)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header value. It returns ok=false for a missing header, a non-Bearer scheme,
// or an empty token. The scheme match is case-insensitive per RFC 7235.
func bearerToken(header string) (token string, ok bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token = strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
