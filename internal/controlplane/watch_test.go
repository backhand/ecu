package controlplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/store"
	"github.com/coder/websocket"
)

// newWatchServer builds a Server with a fixed signing key, a public base URL,
// and (via the dev-toolserver seam) a direct transport pointing at toolURL, so
// the watch proxy resolves to a fake tool server WITHOUT a real tunnel. A
// ready session "s_watch" is created pointing at toolURL.
func newWatchServer(t *testing.T, st *store.Store, toolURL string) (*Server, string) {
	t.Helper()
	srv := NewServer(st, toolURL,
		WithSigningKey("test-signing-key-do-not-use"),
		WithPublicBaseURL("https://watch.example.com"),
	)
	id := "s_watch"
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusReady, ToolEndpoint: toolURL,
		Width: 1280, Height: 800,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return srv, id
}

// TestViewTokenRoundTrip verifies a freshly minted token validates for its own
// session and is rejected for a different session, when tampered, and when
// expired.
func TestViewTokenRoundTrip(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "", WithSigningKey("k"))

	tok := srv.mintViewToken("s_a")
	if !srv.validateViewToken(tok, "s_a") {
		t.Fatal("fresh token did not validate for its own session")
	}
	if srv.validateViewToken(tok, "s_b") {
		t.Fatal("token for s_a must NOT validate for s_b (session scope)")
	}
	if srv.validateViewToken(tok+"x", "s_a") {
		t.Fatal("tampered token must not validate")
	}
	if srv.validateViewToken("", "s_a") {
		t.Fatal("empty token must not validate")
	}
	if srv.validateViewToken("not-base64!!", "s_a") {
		t.Fatal("garbage token must not validate")
	}

	// A token signed with a DIFFERENT key must not validate.
	other := NewServer(st, "", WithSigningKey("different-key"))
	otherTok := other.mintViewToken("s_a")
	if srv.validateViewToken(otherTok, "s_a") {
		t.Fatal("token signed with a different key must not validate")
	}
}

// TestViewTokenExpiry verifies expiry is enforced using an injected clock.
func TestViewTokenExpiry(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	clock := func() time.Time { return now }
	srv := NewServer(st, "", WithSigningKey("k"), WithClock(clock))

	tok := srv.mintViewToken("s_a") // expires at now+TTL
	if !srv.validateViewToken(tok, "s_a") {
		t.Fatal("token should be valid immediately after minting")
	}
	// Advance the clock past the TTL.
	now = now.Add(viewTokenTTL + time.Second)
	if srv.validateViewToken(tok, "s_a") {
		t.Fatal("token must be rejected after it expires")
	}
}

// TestWatchPathExempt verifies the auth-exemption predicate matches exactly the
// watch routes and nothing else (so we never accidentally expose other
// /sessions routes without an API key).
func TestWatchPathExempt(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/sessions/s_abc/watch", true},
		{"/sessions/s_abc/watch/vnc.html", true},
		{"/sessions/s_abc/watch/app/ui.js", true},
		{"/sessions/s_abc/watch/websockify", true},
		{"/sessions/s_abc", false},           // status route — API key required
		{"/sessions/s_abc/click", false},     // action route — API key required
		{"/sessions/s_abc/watchlist", false}, // not the watch segment
		{"/sessions", false},                 // create route
		{"/sessions/", false},                // no id
		{"/agent/connect", false},            // handled by its own exemption
		{"/sessions/s_abc/watcher/x", false}, // not the watch segment
	}
	for _, c := range cases {
		if got := watchPathExempt(c.path); got != c.want {
			t.Errorf("watchPathExempt(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestWatchURLInStatus verifies GET /sessions/{id} returns a watch_url for a
// ready session and that the embedded token validates.
func TestWatchURLInStatus(t *testing.T) {
	st := newTestStore(t)
	srv, id := newWatchServer(t, st, "http://127.0.0.1:1")

	rec := doRequest(t, srv, http.MethodGet, "/sessions/"+id, "Bearer k_active")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"watch_url"`) || strings.Contains(body, `"watch_url":null`) {
		t.Fatalf("expected a non-null watch_url, got %s", body)
	}
	if !strings.Contains(body, "https://watch.example.com/sessions/"+id+"/watch?token=") {
		t.Fatalf("watch_url not in expected form: %s", body)
	}
}

// TestWatchURLNullForNonReady verifies a provisioning session has a null
// watch_url (no live tunnel to watch).
func TestWatchURLNullForNonReady(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "", WithSigningKey("k"), WithPublicBaseURL("https://x"))
	id, _ := store.NewSessionID()
	_ = st.CreateSession(&store.Session{ID: id, Account: "admin", Status: statusProvisioning, Width: 1, Height: 1})

	rec := doRequest(t, srv, http.MethodGet, "/sessions/"+id, "Bearer k_active")
	if !strings.Contains(rec.Body.String(), `"watch_url":null`) {
		t.Fatalf("provisioning session should have null watch_url, got %s", rec.Body.String())
	}
}

// TestWatchProxyTokenGating drives the full watch HTTP proxy: a good token
// yields the fake tool server's noVNC body AND sets the watch cookie; a bad/no
// token is rejected with 403; a valid cookie alone (no token) is accepted.
func TestWatchProxyTokenGating(t *testing.T) {
	// Fake tool server: serves a recognizable noVNC-ish body under /watch.
	const novncBody = "<html><title>noVNC</title></html>"
	var gotPath string
	tool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, novncBody)
	}))
	defer tool.Close()

	st := newTestStore(t)
	srv, id := newWatchServer(t, st, tool.URL)
	handler := srv.Handler()

	goodTok := srv.mintViewToken(id)

	// 1. Good token -> 200 with the noVNC body and a Set-Cookie.
	req := httptest.NewRequest(http.MethodGet, "/sessions/"+id+"/watch/vnc.html?token="+goodTok, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("good-token watch status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != novncBody {
		t.Fatalf("watch body = %q, want noVNC body %q", rec.Body.String(), novncBody)
	}
	if gotPath != "/watch/vnc.html" {
		t.Fatalf("tool server saw path %q, want /watch/vnc.html (prefix preserved)", gotPath)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == watchCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("expected a watch cookie to be set on the token-valid request")
	}
	if cookie.Path != "/sessions/"+id+"/watch" {
		t.Fatalf("watch cookie path = %q, want session-scoped", cookie.Path)
	}

	// 2. No token, but the cookie from step 1 -> accepted.
	req2 := httptest.NewRequest(http.MethodGet, "/sessions/"+id+"/watch/app/ui.js", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("cookie-authed watch status = %d, want 200", rec2.Code)
	}

	// 3. No token and no cookie -> 403.
	req3 := httptest.NewRequest(http.MethodGet, "/sessions/"+id+"/watch/vnc.html", nil)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("no-credential watch status = %d, want 403", rec3.Code)
	}

	// 4. A token for a DIFFERENT session must not open this one.
	wrongTok := srv.mintViewToken("s_other")
	req4 := httptest.NewRequest(http.MethodGet, "/sessions/"+id+"/watch/vnc.html?token="+wrongTok, nil)
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusForbidden {
		t.Fatalf("wrong-session token status = %d, want 403", rec4.Code)
	}
}

// TestWatchProxyRedirectRewrite verifies the CP rewrites a tool-server /watch
// redirect (Location + websockify path query) into the public
// /sessions/{id}/watch space, so the browser stays within the proxied prefix.
func TestWatchProxyRedirectRewrite(t *testing.T) {
	// Fake tool server: mimics the real entry redirect to vnc.html with a
	// tool-server-local /watch prefix and a path=watch/websockify query.
	tool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/watch/vnc.html?autoconnect=true&path=watch/websockify", http.StatusFound)
	}))
	defer tool.Close()

	st := newTestStore(t)
	srv, id := newWatchServer(t, st, tool.URL)
	handler := srv.Handler()
	tok := srv.mintViewToken(id)

	req := httptest.NewRequest(http.MethodGet, "/sessions/"+id+"/watch?token="+tok, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	wantPathPrefix := "/sessions/" + id + "/watch/vnc.html"
	if !strings.HasPrefix(loc, wantPathPrefix) {
		t.Fatalf("redirect Location = %q, want it rewritten under %q", loc, wantPathPrefix)
	}
	if !strings.Contains(loc, "path=sessions%2F"+id+"%2Fwatch%2Fwebsockify") &&
		!strings.Contains(loc, "path=sessions/"+id+"/watch/websockify") {
		t.Fatalf("redirect Location did not rewrite the websockify path query: %q", loc)
	}
}

// TestWatchProxyWebSocket proves a WebSocket upgrade rides the watch proxy end
// to end: a fake tool server accepts a WS at /watch/websockify and echoes a
// frame; the CP proxies the upgrade (101) and the bytes through its transport.
func TestWatchProxyWebSocket(t *testing.T) {
	// Fake tool server speaking WebSocket at /watch/websockify (stands in for
	// noVNC's websockify). It echoes one binary message.
	tool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/watch/websockify" {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		typ, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, append([]byte("echo:"), data...))
	}))
	defer tool.Close()

	st := newTestStore(t)
	srv, id := newWatchServer(t, st, tool.URL)

	// Run the CP handler on a real server so we can dial a real ws:// to it.
	cp := httptest.NewServer(srv.Handler())
	defer cp.Close()

	tok := srv.mintViewToken(id)
	wsURL := "ws" + strings.TrimPrefix(cp.URL, "http") + "/sessions/" + id + "/watch/websockify?token=" + tok

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		status := -1
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("ws dial through watch proxy failed (status=%d): %v", status, err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	if err := c.Write(ctx, websocket.MessageBinary, []byte("hello")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if string(data) != "echo:hello" {
		t.Fatalf("ws echo = %q, want %q (frame did not cross the proxy intact)", data, "echo:hello")
	}
}
