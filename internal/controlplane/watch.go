package controlplane

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Component 9: the live watch endpoint. A human points a browser at the
// session's watch_url; the control plane proxies the request (HTTP for the
// noVNC page/assets AND the WebSocket upgrade for websockify) through that
// session's reverse tunnel to the tool server's /watch, which in turn
// reverse-proxies the container's noVNC on :6080. No inbound port is opened on
// the instance.
//
// This path is API-key-EXEMPT (it is browser-facing) and gated instead by a
// short-lived, session-scoped, HMAC-signed VIEW TOKEN (see viewToken below). It
// is strictly separate from the agent's /screenshot perception path: watch is a
// streaming VNC feed for human eyeballs, screenshot is discrete stills for the
// model.

const (
	// watchPrefix is the public route prefix. watchToolPath is what the tool
	// server serves noVNC under; the two match so the websockify path the
	// browser computes (relative to the page) lines up end to end.
	watchToolPath = "/watch"

	// viewTokenTTL bounds how long a minted view token (and the watch cookie) is
	// valid. Short on purpose: a watch_url is handed out per status poll and is
	// only meant to open a viewer promptly, not to be a durable credential.
	viewTokenTTL = 10 * time.Minute

	// watchCookieName is the short-lived cookie set on the first token-valid
	// request so the browser's subsequent asset/WebSocket requests (which carry
	// no ?token) still authenticate. It is scoped to the session's watch path.
	watchCookieName = "ecu_watch"
)

// watchTokenField separates the three fields packed into a view token.
const watchTokenSep = "|"

// signingKey is the HMAC secret for view tokens. It is set once at construction
// (NewServer) from ECU_SIGNING_KEY or a random 32 bytes. A random key means
// tokens do not survive a control-plane restart — fine for a minutes-long view
// token (the client just re-fetches watch_url).
//
// mintViewToken builds: base64url( sessionID | expiryUnix | hex(HMAC) ), where
// HMAC = HMAC-SHA256(secret, sessionID | expiryUnix). The token therefore binds
// the session id AND the expiry into the signature, so it cannot be replayed for
// another session or after it expires, and cannot be forged without the secret.

// resolveSigningKey returns the configured signing key bytes, or a freshly
// generated random 32-byte key when cfgKey is empty. The boolean reports whether
// a random key was generated (so the caller can log the restart caveat).
func resolveSigningKey(cfgKey string) (key []byte, generated bool) {
	if cfgKey != "" {
		return []byte(cfgKey), false
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		// crypto/rand failing is catastrophic and essentially never happens; a
		// non-random key would undermine the token, so fail loudly by panicking
		// at startup rather than silently weakening security.
		panic(fmt.Sprintf("ecu: generating watch signing key: %v", err))
	}
	return k, true
}

// mintViewToken returns a signed, session-scoped view token that expires at
// now+viewTokenTTL.
func (s *Server) mintViewToken(sessionID string) string {
	expiry := s.now().Add(viewTokenTTL).Unix()
	return s.signViewToken(sessionID, expiry)
}

// signViewToken builds the token for a given session id and absolute expiry.
func (s *Server) signViewToken(sessionID string, expiry int64) string {
	payload := sessionID + watchTokenSep + strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, s.signingKey)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	raw := payload + watchTokenSep + sig
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// validateViewToken reports whether token is a well-formed, correctly-signed,
// unexpired token for sessionID. Every failure mode (malformed, bad signature,
// expired, wrong session) returns false; the caller maps that to 401/403. The
// signature check is constant-time.
func (s *Server) validateViewToken(token, sessionID string) bool {
	if token == "" {
		return false
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	parts := strings.Split(string(rawBytes), watchTokenSep)
	if len(parts) != 3 {
		return false
	}
	gotSession, expiryStr := parts[0], parts[1]

	// Recompute the expected token over the embedded session|expiry and compare
	// the FULL signed token in constant time. Comparing the whole token (not just
	// the signature field) means any tampering with the session or expiry also
	// fails the compare, so a valid compare authenticates BOTH fields at once.
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return false
	}
	expected := s.signViewToken(gotSession, expiry)
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return false
	}
	// Signature is valid; enforce the bindings. The session field is already
	// authenticated, so this just checks it is the session being requested.
	if subtle.ConstantTimeCompare([]byte(gotSession), []byte(sessionID)) != 1 {
		return false
	}
	if s.now().Unix() >= expiry {
		return false
	}
	return true
}

// watchURLFor builds the absolute watch_url for a session: a fresh view token
// appended to <publicBase>/sessions/{id}/watch. Returns "" if no public base is
// configured (then GET /sessions returns watch_url: null).
func (s *Server) watchURLFor(sessionID string) string {
	if s.publicBaseURL == "" {
		return ""
	}
	token := s.mintViewToken(sessionID)
	base := strings.TrimRight(s.publicBaseURL, "/")
	return fmt.Sprintf("%s/sessions/%s/watch?token=%s", base, sessionID, url.QueryEscape(token))
}

// watchPathExempt reports whether path is a live-watch route
// (/sessions/{id}/watch and everything under it). Like /agent/connect it is
// EXEMPT from API-key auth: it is browser-facing and gated by a view token /
// cookie inside handleWatch instead. Matching on the "/watch" segment after a
// /sessions/{id} prefix keeps every existing /sessions auth test unchanged.
func watchPathExempt(path string) bool {
	if !strings.HasPrefix(path, "/sessions/") {
		return false
	}
	// r.URL.Path carries no query string, so we match on path segments only.
	// rest is "{id}/watch" or "{id}/watch/...". Require a non-empty id segment
	// followed by exactly the "watch" segment.
	rest := strings.TrimPrefix(path, "/sessions/")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return false
	}
	tail := rest[slash+1:]
	return tail == "watch" || strings.HasPrefix(tail, "watch/")
}

// handleWatch serves the live noVNC viewer for a human, proxied through the
// session's reverse tunnel to the tool server's /watch (HTTP + WebSocket).
//
// Auth: a valid view token in ?token OR the watch cookie. The token is
// session-scoped and expires (see validateViewToken). On the FIRST token-valid
// request we set the watch cookie (scoped to /sessions/{id}/watch) so the
// browser's follow-up asset/WebSocket requests — which carry no ?token —
// authenticate via the cookie. Missing/bad/expired token AND no valid cookie ->
// 403.
//
// The handler matches "GET /sessions/{id}/watch/{rest...}"; the bare
// "/sessions/{id}/watch" is matched by a sibling route that calls this with an
// empty rest.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Authn: query token first, then cookie. Setting the cookie only happens for
	// a query-token-authenticated request (we never mint trust from a cookie we
	// didn't issue — the cookie value is itself a signed token, so it is
	// re-validated every request).
	queryTok := r.URL.Query().Get("token")
	authedByQuery := s.validateViewToken(queryTok, id)

	authed := authedByQuery
	if !authed {
		if c, err := r.Cookie(watchCookieName); err == nil {
			authed = s.validateViewToken(c.Value, id)
		}
	}
	if !authed {
		// Missing/bad/expired token and no valid cookie. 403: the request reached
		// a real session route but presented no acceptable credential.
		writeError(w, http.StatusForbidden, "missing or invalid watch token")
		return
	}

	// Session must exist and be ready (have a live tunnel to proxy through).
	sess, found, err := s.store.GetSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}
	if sess.Status != statusReady {
		writeError(w, http.StatusConflict, "session is not ready")
		return
	}

	// Resolve the per-session RoundTripper through the tunnel seam (same seam the
	// action proxy uses). The tool-server address never appears on this side.
	rt, ok := s.transport.RoundTripper(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}

	// On a query-token request, set the short-lived cookie scoped to this
	// session's watch path so subsequent asset/WS requests authenticate without
	// the token in the URL. We reuse the SAME signed token as the cookie value:
	// it is self-contained, session-scoped, and expiring, so it needs no server
	// state and is re-validated on every request.
	if authedByQuery {
		http.SetCookie(w, &http.Cookie{
			Name:     watchCookieName,
			Value:    queryTok,
			Path:     "/sessions/" + id + "/watch",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   r.TLS != nil,
			MaxAge:   int(viewTokenTTL.Seconds()),
		})
	}

	// Build the upstream path on the tool server. Everything under
	// /sessions/{id}/watch maps onto the tool server's /watch[/...]. The tool
	// server then strips its own /watch prefix and forwards to noVNC.
	rest := r.PathValue("rest")
	upstreamPath := watchToolPath
	if rest != "" {
		upstreamPath = watchToolPath + "/" + rest
	}

	// A single reverse proxy handles BOTH plain HTTP (the noVNC page + assets)
	// AND the websockify WebSocket upgrade: httputil.ReverseProxy hijacks an
	// Upgrade response and pipes the connection over the transport's dialed conn
	// — which here is a yamux stream to the agent, spliced byte-for-byte to the
	// tool server. So the 101 and the RFB frames ride the tunnel unchanged.
	proxy := &httputil.ReverseProxy{
		Transport: rt,
		Director: func(out *http.Request) {
			// The RoundTripper rewrites scheme/host before dialing; we set a
			// placeholder host (never seen by the client) and the upstream path.
			out.URL.Scheme = "http"
			out.URL.Host = "tool-server"
			out.URL.Path = upstreamPath
			out.Host = "tool-server"
			// Strip our auth artifacts from the upstream query: the tool server
			// doesn't need the view token, and noVNC reads its OWN params
			// (autoconnect/path/...) which the tool server's entry redirect adds.
			q := out.URL.Query()
			q.Del("token")
			out.URL.RawQuery = q.Encode()
		},
		ModifyResponse: func(resp *http.Response) error {
			// The tool server is prefix-AGNOSTIC: its /watch entry point redirects
			// to "/watch/vnc.html?...&path=watch/websockify" (a tool-server-local
			// /watch prefix). The browser, though, is at /sessions/{id}/watch, so
			// we must translate that tool-server prefix back to the public one in
			// any redirect Location: rewrite a leading "/watch" -> the public
			// "/sessions/{id}/watch" AND the "path=watch/..." query (which noVNC
			// uses to build the websockify URL) -> "path=sessions/{id}/watch/...".
			// This is the single seam where the two prefixes are reconciled.
			rewriteWatchLocation(resp, id)
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, e error) {
			// Never surface the underlying error (it may name the upstream
			// host:port). A dead tunnel / unreachable tool server -> 502.
			log.Printf("ecu watch: proxy error for session %s: %v", id, e)
			writeError(rw, http.StatusBadGateway, "watch upstream unreachable")
		},
	}
	proxy.ServeHTTP(w, r)
}

// rewriteWatchLocation translates a tool-server-local /watch redirect into the
// public /sessions/{id}/watch space. It rewrites a leading "/watch" in the
// Location path to "/sessions/{id}/watch", and the noVNC websockify "path"
// query from "watch/..." to "sessions/{id}/watch/..." so the viewer connects its
// WebSocket back through the control plane (not to a bare /watch the CP doesn't
// serve). A non-redirect response, or a Location that isn't under /watch, is
// left untouched.
func rewriteWatchLocation(resp *http.Response, sessionID string) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		return
	}
	publicPrefix := "/sessions/" + sessionID + "/watch"

	// Rewrite the path: "/watch" or "/watch/..." -> public prefix (+ rest).
	if u.Path == watchToolPath {
		u.Path = publicPrefix
	} else if strings.HasPrefix(u.Path, watchToolPath+"/") {
		u.Path = publicPrefix + strings.TrimPrefix(u.Path, watchToolPath)
	}

	// Rewrite the websockify path query: noVNC wants it relative to the page
	// host with no leading slash, so "watch/websockify" -> "sessions/{id}/watch/
	// websockify".
	q := u.Query()
	if p := q.Get("path"); p == "watch" || strings.HasPrefix(p, "watch/") {
		q.Set("path", "sessions/"+sessionID+"/"+p)
		u.RawQuery = q.Encode()
	}

	resp.Header.Set("Location", u.String())
}
