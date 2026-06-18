package controlplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/backhand/ecu/internal/store"
)

// TestProxyPassthrough stands up a fake tool server and verifies that the proxy
// forwards an action request to it and copies the status code, content-type,
// and body back verbatim — and, critically, that the upstream host:port never
// appears in the client-facing response (body or any header).
func TestProxyPassthrough(t *testing.T) {
	// Fake Component-1 tool server. It echoes a recognizable body and asserts
	// it received the request at the expected path with the expected body.
	const upstreamBody = `{"ok":true,"echo":"pong"}`
	var gotPath, gotBody, gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot) // distinctive status to prove passthrough
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	upstreamHost := upstreamURL.Host // host:port we must never leak

	// Store with an active key and a ready session pointing at the fake server.
	st := newTestStore(t)
	srv := NewServer(st, "")

	id, _ := store.NewSessionID()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusReady, ToolEndpoint: upstream.URL,
		Width: 1280, Height: 800,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Issue a click action through the control plane.
	reqBody := `{"x":10,"y":20,"button":"left"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+id+"/click", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer k_active")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Verbatim status + body + content-type.
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d (verbatim upstream status)", rec.Code, http.StatusTeapot)
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Fatalf("body = %q, want verbatim %q", got, upstreamBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json (copied from upstream)", ct)
	}

	// The upstream got the request at /click with the original body + CT.
	if gotPath != "/click" {
		t.Fatalf("upstream path = %q, want /click", gotPath)
	}
	if gotBody != reqBody {
		t.Fatalf("upstream body = %q, want %q (forwarded verbatim)", gotBody, reqBody)
	}
	if gotCT != "application/json" {
		t.Fatalf("upstream content-type = %q, want application/json", gotCT)
	}

	// No leak: the upstream host:port must not appear in the response body or
	// any response header value.
	if strings.Contains(rec.Body.String(), upstreamHost) {
		t.Fatalf("response body leaks upstream host %q: %s", upstreamHost, rec.Body.String())
	}
	for name, vals := range rec.Header() {
		for _, v := range vals {
			if strings.Contains(v, upstreamHost) {
				t.Fatalf("response header %q leaks upstream host %q: %q", name, upstreamHost, v)
			}
		}
	}
}

// TestProxyForwardsActions verifies the batch/macro endpoint (protocol #5) is
// admitted by allowedActions and proxied verbatim to the upstream tool server
// at /actions — i.e. it is NOT rejected as an "unknown action". Mirrors the
// TestProxyPassthrough setup (fake upstream + ready session).
func TestProxyForwardsActions(t *testing.T) {
	// Sanity: "actions" is in the allowed set (so it forwards, never 400s).
	if !allowedActions["actions"] {
		t.Fatalf("allowedActions[\"actions\"] = false, want true (batch endpoint)")
	}

	const upstreamBody = `{"results":[{"ok":true},{"ok":true}],"screenshot":{"mode":"full"}}`
	var gotPath, gotBody, gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	st := newTestStore(t)
	srv := NewServer(st, "")

	id, _ := store.NewSessionID()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusReady, ToolEndpoint: upstream.URL,
		Width: 1280, Height: 800,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// A batch request: two actions + a trailing screenshot.
	reqBody := `{"actions":[{"action":"click","x":10,"y":20},{"action":"key","keys":"Return"}],"screenshot":{"format":"png"}}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+id+"/actions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer k_active")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// NOT a 400 unknown-action — the batch endpoint is forwarded.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400 (unknown action); /actions must be forwarded, not rejected")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (verbatim upstream status)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Fatalf("body = %q, want verbatim %q", got, upstreamBody)
	}

	// The upstream received the request at /actions with the original body + CT.
	if gotPath != "/actions" {
		t.Fatalf("upstream path = %q, want /actions (forwarded to the batch endpoint)", gotPath)
	}
	if gotBody != reqBody {
		t.Fatalf("upstream body = %q, want %q (forwarded verbatim)", gotBody, reqBody)
	}
	if gotCT != "application/json" {
		t.Fatalf("upstream content-type = %q, want application/json", gotCT)
	}
}

// TestProxyUnknownAction verifies an unsupported action yields 400 with JSON
// detail and never touches a tool server.
func TestProxyUnknownAction(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "")
	id, _ := store.NewSessionID()
	_ = st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusReady, ToolEndpoint: "http://127.0.0.1:1", Width: 1, Height: 1,
	})

	rec := doRequest(t, srv, http.MethodPost, "/sessions/"+id+"/frobnicate", "Bearer k_active")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown action", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

// TestProxyNotReady verifies a known but non-ready session yields 409.
func TestProxyNotReady(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "")
	id, _ := store.NewSessionID()
	_ = st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusProvisioning, ToolEndpoint: "", Width: 1, Height: 1,
	})

	rec := doRequest(t, srv, http.MethodPost, "/sessions/"+id+"/click", "Bearer k_active")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for non-ready session", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

// TestProxyUnknownSession verifies an unknown session yields 404 on the action
// path.
func TestProxyUnknownSession(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "")
	rec := doRequest(t, srv, http.MethodPost, "/sessions/s_missing/click", "Bearer k_active")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown session", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}
