package controlplane

import (
	"context"
	"net/http"
	"strings"
)

// bakeCallbackExempt reports whether path is the C7 bake-completion callback
// (POST /internal/bake/{token}/done). Like agentConnectPath it is EXEMPT from
// API-key auth: a bake instance has no API key and authenticates with its
// per-bake token, checked (constant-time) in handleBakeDone. The check is a
// prefix+suffix match on the dynamic {token} segment so it covers every token.
func bakeCallbackExempt(path string) bool {
	return strings.HasPrefix(path, bakeCallbackPrefix) && strings.HasSuffix(path, "/done")
}

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

// agentConnectPath is the tunnel-ingress route that authenticates with a
// per-session tunnel token instead of an API key. authMiddleware exempts it so
// the API-key check does not reject agents; the handler enforces its own
// constant-time token check before upgrading the WebSocket.
const agentConnectPath = "/agent/connect"

// authMiddleware validates the Authorization: Bearer <key> header against the
// store and, on success, attaches the resolved account to the request context.
// Every failure mode — missing header, malformed header, empty key, unknown
// key, and disabled key — is rejected with 401 and a JSON {"detail": ...} body.
// It wraps the entire mux so all routes are authenticated EXCEPT two
// token-authed endpoints: the tunnel ingress agentConnectPath (tunnel-token auth,
// see broker.go) and the C7 bake-completion callback (per-bake-token auth, see
// bake.go). Both do their own constant-time token check in their handlers. Path
// checks are preferred over splitting the mux because they leave the existing
// method-based /sessions routing — and every auth test that exercises it —
// completely unchanged.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == agentConnectPath || bakeCallbackExempt(r.URL.Path) {
			// Token-authed endpoints (tunnel ingress / bake-completion callback):
			// API-key auth does not apply; each handler does its own constant-time
			// per-operation token check.
			next.ServeHTTP(w, r)
			return
		}
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
