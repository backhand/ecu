package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/store"
)

// newTestStore opens a temp-file store seeded with one active key ("k_active",
// account "admin") and one disabled key ("k_disabled").
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ecu.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.SeedBootstrapKey("k_active"); err != nil {
		t.Fatalf("seed active key: %v", err)
	}
	if err := st.InsertKey(store.APIKey{
		Key: "k_disabled", Account: "ops", Status: "disabled", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed disabled key: %v", err)
	}
	return st
}

// doRequest runs a single request through the server handler and returns the
// recorder.
func doRequest(t *testing.T, srv *Server, method, target, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertJSONDetail asserts the body is a JSON object with a non-empty "detail".
func assertJSONDetail(t *testing.T, body []byte) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, body)
	}
	if d, ok := m["detail"].(string); !ok || d == "" {
		t.Fatalf("response body missing non-empty \"detail\": %s", body)
	}
}

func TestAuthMissingHeader(t *testing.T) {
	srv := NewServer(newTestStore(t), "")
	rec := doRequest(t, srv, http.MethodGet, "/sessions/s_x", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for missing header", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

func TestAuthEmptyKey(t *testing.T) {
	srv := NewServer(newTestStore(t), "")
	rec := doRequest(t, srv, http.MethodGet, "/sessions/s_x", "Bearer ")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for empty key", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

func TestAuthMalformedHeader(t *testing.T) {
	srv := NewServer(newTestStore(t), "")
	rec := doRequest(t, srv, http.MethodGet, "/sessions/s_x", "Token k_active")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for non-Bearer scheme", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

func TestAuthUnknownKey(t *testing.T) {
	srv := NewServer(newTestStore(t), "")
	rec := doRequest(t, srv, http.MethodGet, "/sessions/s_x", "Bearer k_nope")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for unknown key", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

func TestAuthDisabledKey(t *testing.T) {
	srv := NewServer(newTestStore(t), "")
	rec := doRequest(t, srv, http.MethodGet, "/sessions/s_x", "Bearer k_disabled")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for disabled key", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())
}

// TestAuthValidKeyPassesThrough verifies a valid active key is accepted: the
// middleware attaches the account to the context, and end-to-end a valid key on
// an unknown session yields 404 (i.e. it passed auth) rather than 401.
func TestAuthValidKeyPassesThrough(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "")

	// Probe the context plumbing directly via the middleware.
	var gotAccount string
	var sawAccount bool
	probe := srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccount, sawAccount = accountFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer k_active")
	rec := httptest.NewRecorder()
	probe.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid key did not reach handler: status = %d", rec.Code)
	}
	if !sawAccount || gotAccount != "admin" {
		t.Fatalf("account in context = %q (present=%v), want admin", gotAccount, sawAccount)
	}

	// End-to-end: a valid key on an unknown session yields 404 (passed auth).
	rec2 := doRequest(t, srv, http.MethodGet, "/sessions/s_missing", "Bearer k_active")
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("valid key on unknown session: status = %d, want 404", rec2.Code)
	}
}
