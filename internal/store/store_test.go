package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// Registers the "sqlite" driver for the raw connection used in the
	// migration test.
	_ "modernc.org/sqlite"
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
// unknown keys, including the C8 persistent_allowed capability return.
func TestLookupKeyActiveAndDisabled(t *testing.T) {
	st := openTestStore(t)

	// Seeded bootstrap key is active AND persistent-allowed (SeedBootstrapKey
	// seeds persistent_allowed=1).
	if err := st.SeedBootstrapKey("k_active"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Insert a disabled key directly (persistent_allowed=0).
	mustExec(t, st, `INSERT INTO api_keys (key, account, status, persistent_allowed, created_at) VALUES (?, ?, ?, ?, ?)`,
		"k_disabled", "ops", "disabled", 0, time.Now().UTC().Format(time.RFC3339Nano))
	// Insert an active key WITHOUT the persistent capability.
	mustExec(t, st, `INSERT INTO api_keys (key, account, status, persistent_allowed, created_at) VALUES (?, ?, ?, ?, ?)`,
		"k_noperist", "ops", "active", 0, time.Now().UTC().Format(time.RFC3339Nano))

	account, active, persistentAllowed, found, err := st.LookupKey("k_active")
	if err != nil {
		t.Fatalf("LookupKey active: %v", err)
	}
	if !found || !active || account != "admin" || !persistentAllowed {
		t.Fatalf("active key: found=%v active=%v account=%q persistentAllowed=%v, want true/true/admin/true", found, active, account, persistentAllowed)
	}

	_, active, _, found, err = st.LookupKey("k_disabled")
	if err != nil {
		t.Fatalf("LookupKey disabled: %v", err)
	}
	if !found || active {
		t.Fatalf("disabled key: found=%v active=%v, want found=true active=false", found, active)
	}

	// Active but not persistent-allowed.
	_, active, persistentAllowed, found, err = st.LookupKey("k_noperist")
	if err != nil {
		t.Fatalf("LookupKey k_noperist: %v", err)
	}
	if !found || !active || persistentAllowed {
		t.Fatalf("non-persistent key: found=%v active=%v persistentAllowed=%v, want true/true/false", found, active, persistentAllowed)
	}

	_, _, _, found, err = st.LookupKey("k_nope")
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

// TestSessionByTunnelToken verifies token-based session lookup: a matching
// token resolves the session, an empty token never matches (even though
// dev-mode rows store an empty token), and an unknown token returns found=false.
func TestSessionByTunnelToken(t *testing.T) {
	st := openTestStore(t)

	tok, err := NewTunnelToken()
	if err != nil {
		t.Fatalf("NewTunnelToken: %v", err)
	}
	if !strings.HasPrefix(tok, "t_") || len(tok) != len("t_")+64 {
		t.Fatalf("tunnel token %q not in expected t_+64hex form", tok)
	}

	id, _ := NewSessionID()
	if err := st.CreateSession(&Session{
		ID: id, Account: "admin", Status: "provisioning", ToolEndpoint: "",
		Width: 1280, Height: 800, TunnelToken: tok,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Also create a dev-mode-style session with an EMPTY token to prove empty
	// tokens never authenticate against the default value.
	idEmpty, _ := NewSessionID()
	if err := st.CreateSession(&Session{
		ID: idEmpty, Account: "admin", Status: "ready", ToolEndpoint: "http://x",
		Width: 1, Height: 1, TunnelToken: "",
	}); err != nil {
		t.Fatalf("CreateSession (empty token): %v", err)
	}

	// Matching token resolves the right session and round-trips the token.
	got, found, err := st.SessionByTunnelToken(tok)
	if err != nil || !found {
		t.Fatalf("SessionByTunnelToken(valid): found=%v err=%v", found, err)
	}
	if got.ID != id || got.TunnelToken != tok {
		t.Fatalf("resolved wrong session: id=%q token=%q", got.ID, got.TunnelToken)
	}

	// Empty token never matches.
	if _, found, err := st.SessionByTunnelToken(""); err != nil || found {
		t.Fatalf("SessionByTunnelToken(\"\"): found=%v err=%v, want found=false", found, err)
	}

	// Unknown token never matches.
	if _, found, err := st.SessionByTunnelToken("t_deadbeef"); err != nil || found {
		t.Fatalf("SessionByTunnelToken(unknown): found=%v err=%v, want found=false", found, err)
	}
}

// TestTunnelTokenMigration verifies the idempotent ALTER path: a sessions table
// created WITHOUT tunnel_token (simulating a pre-C3 DB) is migrated on Open so
// the column exists and token operations work.
func TestTunnelTokenMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Hand-create a pre-C3 sessions table (no tunnel_token column) by opening a
	// raw connection through the same driver and running the old schema.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	const legacySessions = `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, account TEXT NOT NULL, status TEXT NOT NULL,
    tool_endpoint TEXT NOT NULL, persistent INTEGER NOT NULL,
    width INTEGER NOT NULL, height INTEGER NOT NULL,
    created_at TEXT NOT NULL, last_activity_at TEXT NOT NULL
);`
	if _, err := raw.Exec(legacySessions); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	raw.Close()

	// Open via the store: the migration must add tunnel_token.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrating): %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A token-bearing session now works end-to-end.
	tok, _ := NewTunnelToken()
	id, _ := NewSessionID()
	if err := st.CreateSession(&Session{
		ID: id, Account: "admin", Status: "provisioning", ToolEndpoint: "",
		Width: 1, Height: 1, TunnelToken: tok,
	}); err != nil {
		t.Fatalf("CreateSession after migration: %v", err)
	}
	got, found, err := st.SessionByTunnelToken(tok)
	if err != nil || !found || got.ID != id {
		t.Fatalf("post-migration token lookup failed: found=%v err=%v", found, err)
	}

	// Opening again must be a no-op (idempotent) and not error.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent migration): %v", err)
	}
	st2.Close()
}

// TestPersistColumnsMigrationFromLegacy verifies the C8 idempotent ALTER path: a
// sessions table created WITHOUT snapshot_image / stopped_at (a pre-C8 DB) is
// migrated on Open so both columns exist and a persistent stop round-trips. It
// mirrors TestTunnelTokenMigration (which created a table missing tunnel_token);
// here we use a post-C5 legacy table that already has tunnel_lost_at but lacks
// the two C8 columns.
func TestPersistColumnsMigrationFromLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_c5.db")

	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	// A post-C5 table: has tunnel_token / instance_id / tunnel_lost_at but NOT the
	// two C8 columns.
	const legacySessions = `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, account TEXT NOT NULL, status TEXT NOT NULL,
    tool_endpoint TEXT NOT NULL, persistent INTEGER NOT NULL,
    width INTEGER NOT NULL, height INTEGER NOT NULL,
    created_at TEXT NOT NULL, last_activity_at TEXT NOT NULL,
    tunnel_token TEXT NOT NULL DEFAULT '', instance_id TEXT NOT NULL DEFAULT '',
    tunnel_lost_at TEXT NOT NULL DEFAULT ''
);`
	if _, err := raw.Exec(legacySessions); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	raw.Close()

	// Open via the store: the migration must add snapshot_image + stopped_at.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrating): %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A persistent stop now works end-to-end (would fail at the UPDATE/SELECT if a
	// column were missing).
	id, _ := NewSessionID()
	if err := st.CreateSession(&Session{
		ID: id, Account: "admin", Status: "ready", ToolEndpoint: "",
		Persistent: true, Width: 1, Height: 1,
	}); err != nil {
		t.Fatalf("CreateSession after migration: %v", err)
	}
	if err := st.MarkSessionStopped(id, "fake-image-x"); err != nil {
		t.Fatalf("MarkSessionStopped after migration: %v", err)
	}
	got, found, err := st.GetSession(id)
	if err != nil || !found || got.SnapshotImage != "fake-image-x" || got.StoppedAt.IsZero() {
		t.Fatalf("post-migration stop round-trip failed: found=%v err=%v snap=%q stoppedAt=%v", found, err, got.SnapshotImage, got.StoppedAt)
	}

	// Opening again must be a no-op (idempotent) and not error.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent migration): %v", err)
	}
	st2.Close()
}

// mustExec runs a statement against the store's db for test setup, failing the
// test on error.
func mustExec(t *testing.T, st *Store, query string, args ...any) {
	t.Helper()
	if _, err := st.db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
