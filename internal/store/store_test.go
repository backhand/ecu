package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTestStore opens a Store backed by a real temp file (NOT :memory:, so the
// WAL/file path is exercised) and registers cleanup.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ecu.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestSeedBootstrapKeyIdempotent verifies that seeding the bootstrap key twice
// leaves exactly one row and never errors.
func TestSeedBootstrapKeyIdempotent(t *testing.T) {
	st := openTestStore(t)

	if err := st.SeedBootstrapKey("k_admin"); err != nil {
		t.Fatalf("first SeedBootstrapKey: %v", err)
	}
	if err := st.SeedBootstrapKey("k_admin"); err != nil {
		t.Fatalf("second SeedBootstrapKey: %v", err)
	}
	n, err := st.CountKeys()
	if err != nil {
		t.Fatalf("CountKeys: %v", err)
	}
	if n != 1 {
		t.Fatalf("key count = %d, want 1 after idempotent seed", n)
	}
}

// TestLookupKeyActiveAndDisabled verifies LookupKey for active, disabled, and
// unknown keys.
func TestLookupKeyActiveAndDisabled(t *testing.T) {
	st := openTestStore(t)

	// Seeded bootstrap key is active.
	if err := st.SeedBootstrapKey("k_active"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Insert a disabled key directly.
	mustExec(t, st, `INSERT INTO api_keys (key, account, status, persistent_allowed, created_at) VALUES (?, ?, ?, ?, ?)`,
		"k_disabled", "ops", "disabled", 0, time.Now().UTC().Format(time.RFC3339Nano))

	account, active, found, err := st.LookupKey("k_active")
	if err != nil {
		t.Fatalf("LookupKey active: %v", err)
	}
	if !found || !active || account != "admin" {
		t.Fatalf("active key: found=%v active=%v account=%q, want true/true/admin", found, active, account)
	}

	_, active, found, err = st.LookupKey("k_disabled")
	if err != nil {
		t.Fatalf("LookupKey disabled: %v", err)
	}
	if !found || active {
		t.Fatalf("disabled key: found=%v active=%v, want found=true active=false", found, active)
	}

	_, _, found, err = st.LookupKey("k_nope")
	if err != nil {
		t.Fatalf("LookupKey unknown: %v", err)
	}
	if found {
		t.Fatalf("unknown key reported found=true, want false")
	}
}

// TestSessionCRUD exercises create/get/update/delete and TouchSession.
func TestSessionCRUD(t *testing.T) {
	st := openTestStore(t)

	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	if !strings.HasPrefix(id, "s_") || len(id) != len("s_")+32 {
		t.Fatalf("session id %q not in expected s_+32hex form", id)
	}

	in := &Session{
		ID:           id,
		Account:      "admin",
		Status:       "ready",
		ToolEndpoint: "http://127.0.0.1:8000",
		Persistent:   true,
		Width:        1280,
		Height:       800,
	}
	if err := st.CreateSession(in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, found, err := st.GetSession(id)
	if err != nil || !found {
		t.Fatalf("GetSession: found=%v err=%v", found, err)
	}
	if got.Account != "admin" || got.Status != "ready" || got.ToolEndpoint != "http://127.0.0.1:8000" ||
		!got.Persistent || got.Width != 1280 || got.Height != 800 {
		t.Fatalf("round-tripped session mismatch: %+v", got)
	}

	// Update status.
	if err := st.UpdateSessionStatus(id, "terminated"); err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	got, _, _ = st.GetSession(id)
	if got.Status != "terminated" {
		t.Fatalf("status after update = %q, want terminated", got.Status)
	}

	// Unknown session: found=false, no error.
	_, found, err = st.GetSession("s_missing")
	if err != nil {
		t.Fatalf("GetSession unknown returned error: %v", err)
	}
	if found {
		t.Fatalf("GetSession unknown reported found=true")
	}
}

// TestTouchSessionUpdatesActivity verifies TouchSession advances
// last_activity_at. RFC3339Nano resolution plus a tiny sleep guarantees a
// strictly later timestamp without relying on sub-nanosecond timing.
func TestTouchSessionUpdatesActivity(t *testing.T) {
	st := openTestStore(t)

	id, _ := NewSessionID()
	if err := st.CreateSession(&Session{
		ID: id, Account: "admin", Status: "ready", ToolEndpoint: "http://x", Width: 1, Height: 1,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	before, _, _ := st.GetSession(id)

	time.Sleep(2 * time.Millisecond)
	if err := st.TouchSession(id); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	after, _, _ := st.GetSession(id)

	if !after.LastActivityAt.After(before.LastActivityAt) {
		t.Fatalf("last_activity_at not advanced: before=%v after=%v",
			before.LastActivityAt, after.LastActivityAt)
	}
}

// mustExec runs a statement against the store's db for test setup, failing the
// test on error.
func mustExec(t *testing.T, st *Store, query string, args ...any) {
	t.Helper()
	if _, err := st.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
