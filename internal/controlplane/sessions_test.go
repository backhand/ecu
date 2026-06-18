package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateSessionDevMode verifies that with ECU_DEV_TOOLSERVER set, POST
// /sessions returns a ready session with the contract fields.
func TestCreateSessionDevMode(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000")

	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"persistent":false}`))
	req.Header.Set("Authorization", "Bearer k_active")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.Bytes())
	}
	if !strings.HasPrefix(resp.SessionID, "s_") {
		t.Fatalf("session_id = %q, want s_ prefix", resp.SessionID)
	}
	if resp.Status != statusReady {
		t.Fatalf("status = %q, want ready in dev mode", resp.Status)
	}
	if resp.Width != defaultWidth || resp.Height != defaultHeight {
		t.Fatalf("dims = %dx%d, want %dx%d", resp.Width, resp.Height, defaultWidth, defaultHeight)
	}

	// The response must not expose the tool endpoint.
	if strings.Contains(rec.Body.String(), "127.0.0.1:8000") {
		t.Fatalf("create response leaks tool endpoint: %s", rec.Body.String())
	}
}

// TestCreateSessionNoDevMode verifies that without the dev seam, a session is
// created in the provisioning state (awaiting its reverse tunnel, per C3) and
// still readable via GET. It also confirms the tunnel token is NOT leaked in
// the response by default (only the dev exposure seam surfaces it).
func TestCreateSessionNoDevMode(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "")

	req := httptest.NewRequest(http.MethodPost, "/sessions", nil) // empty body is valid
	req.Header.Set("Authorization", "Bearer k_active")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp createSessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != statusProvisioning {
		t.Fatalf("status = %q, want provisioning when awaiting a tunnel", resp.Status)
	}
	// Without the dev exposure seam, the token/url must be omitted entirely.
	if resp.TunnelToken != "" || resp.TunnelURL != "" {
		t.Fatalf("tunnel token/url leaked without exposure seam: token=%q url=%q", resp.TunnelToken, resp.TunnelURL)
	}
	if strings.Contains(rec.Body.String(), "tunnel_token") {
		t.Fatalf("response unexpectedly contains tunnel_token: %s", rec.Body.String())
	}

	// GET it back.
	rec2 := doRequest(t, srv, http.MethodGet, "/sessions/"+resp.SessionID, "Bearer k_active")
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec2.Code)
	}
	var get getSessionResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &get)
	if get.Status != statusProvisioning {
		t.Fatalf("GET status field = %q, want provisioning", get.Status)
	}
	if get.WatchURL != nil {
		t.Fatalf("watch_url = %v, want null", *get.WatchURL)
	}
}

// TestDeleteSessionIdempotent verifies DELETE terminates a session and a second
// DELETE on the same id still returns terminated; an unknown id returns 404.
func TestDeleteSessionIdempotent(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000")

	// Create one.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer k_active")
	srv.Handler().ServeHTTP(rec, req)
	var created createSessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// First delete.
	d1 := doRequest(t, srv, http.MethodDelete, "/sessions/"+created.SessionID, "Bearer k_active")
	if d1.Code != http.StatusOK {
		t.Fatalf("first DELETE status = %d, want 200", d1.Code)
	}
	if !strings.Contains(d1.Body.String(), statusTerminated) {
		t.Fatalf("first DELETE body = %s, want terminated", d1.Body.String())
	}

	// Second delete on the same (now terminated) id is idempotent-ish.
	d2 := doRequest(t, srv, http.MethodDelete, "/sessions/"+created.SessionID, "Bearer k_active")
	if d2.Code != http.StatusOK {
		t.Fatalf("second DELETE status = %d, want 200 (idempotent-ish)", d2.Code)
	}

	// Unknown id -> 404.
	d3 := doRequest(t, srv, http.MethodDelete, "/sessions/s_missing", "Bearer k_active")
	if d3.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown status = %d, want 404", d3.Code)
	}
	assertJSONDetail(t, d3.Body.Bytes())
}
