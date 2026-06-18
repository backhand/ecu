package store

import (
	"testing"
	"time"
)

// makeSession inserts a session row with the given id, account, and status. It
// returns the created row. CreateSession overwrites created_at/last_activity_at
// with now and forces tunnel_lost_at empty; status is set afterwards via
// UpdateSessionStatus so callers can stage any of the four states.
func makeSession(t *testing.T, st *Store, id, account, status string) *Session {
	t.Helper()
	if err := st.CreateSession(&Session{
		ID: id, Account: account, Status: statusProvisioningSeed,
		Width: 1280, Height: 800,
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
	if status != statusProvisioningSeed {
		if err := st.UpdateSessionStatus(id, status); err != nil {
			t.Fatalf("UpdateSessionStatus(%s,%s): %v", id, status, err)
		}
	}
	sess, found, err := st.GetSession(id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", id, found, err)
	}
	return sess
}

// Status string literals duplicated locally for the store tests (the store
// package does not import the controlplane status constants). These MUST match
// the values the schema and controlplane use.
const (
	statusProvisioningSeed = "provisioning"
	statusReadySeed        = "ready"
	statusErrorSeed        = "error"
	statusTerminatedSeed   = "terminated"
)

// TestListNonTerminalSessions verifies the reaper's per-sweep input includes
// only provisioning + ready rows and excludes error + terminated, ordered by
// creation time.
func TestListNonTerminalSessions(t *testing.T) {
	st := openTestStore(t)

	makeSession(t, st, "s_prov", "admin", statusProvisioningSeed)
	makeSession(t, st, "s_ready", "admin", statusReadySeed)
	makeSession(t, st, "s_err", "admin", statusErrorSeed)
	makeSession(t, st, "s_term", "admin", statusTerminatedSeed)

	got, err := st.ListNonTerminalSessions()
	if err != nil {
		t.Fatalf("ListNonTerminalSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d non-terminal sessions, want 2: %+v", len(got), got)
	}
	seen := map[string]string{}
	for _, s := range got {
		seen[s.ID] = s.Status
	}
	if seen["s_prov"] != statusProvisioningSeed {
		t.Fatalf("provisioning session missing/wrong: %v", seen)
	}
	if seen["s_ready"] != statusReadySeed {
		t.Fatalf("ready session missing/wrong: %v", seen)
	}
	if _, ok := seen["s_err"]; ok {
		t.Fatalf("error session must be excluded from non-terminal list: %v", seen)
	}
	if _, ok := seen["s_term"]; ok {
		t.Fatalf("terminated session must be excluded from non-terminal list: %v", seen)
	}
}

// TestCountActiveSessions verifies the active count (provisioning + ready) used
// by the global cap, and that error/terminated rows do not count.
func TestCountActiveSessions(t *testing.T) {
	st := openTestStore(t)

	if n, err := st.CountActiveSessions(); err != nil || n != 0 {
		t.Fatalf("empty store: CountActiveSessions = %d, err = %v, want 0", n, err)
	}

	makeSession(t, st, "s_prov", "admin", statusProvisioningSeed)
	makeSession(t, st, "s_ready", "admin", statusReadySeed)
	makeSession(t, st, "s_err", "admin", statusErrorSeed)
	makeSession(t, st, "s_term", "admin", statusTerminatedSeed)

	n, err := st.CountActiveSessions()
	if err != nil {
		t.Fatalf("CountActiveSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountActiveSessions = %d, want 2 (provisioning + ready only)", n)
	}
}

// TestCountActiveSessionsForAccount verifies the per-account active count
// partitions by account and respects the active-status filter.
func TestCountActiveSessionsForAccount(t *testing.T) {
	st := openTestStore(t)

	makeSession(t, st, "s_a1", "alice", statusReadySeed)
	makeSession(t, st, "s_a2", "alice", statusProvisioningSeed)
	makeSession(t, st, "s_a3", "alice", statusTerminatedSeed) // not active
	makeSession(t, st, "s_b1", "bob", statusReadySeed)

	if n, err := st.CountActiveSessionsForAccount("alice"); err != nil || n != 2 {
		t.Fatalf("alice active = %d, err = %v, want 2", n, err)
	}
	if n, err := st.CountActiveSessionsForAccount("bob"); err != nil || n != 1 {
		t.Fatalf("bob active = %d, err = %v, want 1", n, err)
	}
	if n, err := st.CountActiveSessionsForAccount("carol"); err != nil || n != 0 {
		t.Fatalf("carol active = %d, err = %v, want 0", n, err)
	}
}

// TestSetSessionTunnelLostRoundTrip verifies tunnel_lost_at sets and clears
// through GetSession: a stamped time reads back ~equal, and a zero time clears
// it back to the zero value.
func TestSetSessionTunnelLostRoundTrip(t *testing.T) {
	st := openTestStore(t)
	makeSession(t, st, "s_x", "admin", statusReadySeed)

	// Fresh row: never lost.
	sess, _, _ := st.GetSession("s_x")
	if !sess.TunnelLostAt.IsZero() {
		t.Fatalf("fresh session TunnelLostAt = %v, want zero", sess.TunnelLostAt)
	}

	// Stamp a loss instant; it must read back equal to nanosecond precision
	// (RFC3339Nano round-trips the full resolution).
	lost := time.Date(2026, 6, 18, 12, 34, 56, 123456789, time.UTC)
	if err := st.SetSessionTunnelLost("s_x", lost); err != nil {
		t.Fatalf("SetSessionTunnelLost: %v", err)
	}
	sess, _, _ = st.GetSession("s_x")
	if !sess.TunnelLostAt.Equal(lost) {
		t.Fatalf("TunnelLostAt = %v, want %v", sess.TunnelLostAt, lost)
	}

	// Clear it with the zero time; GetSession must show the zero value again.
	if err := st.SetSessionTunnelLost("s_x", time.Time{}); err != nil {
		t.Fatalf("SetSessionTunnelLost(clear): %v", err)
	}
	sess, _, _ = st.GetSession("s_x")
	if !sess.TunnelLostAt.IsZero() {
		t.Fatalf("after clear TunnelLostAt = %v, want zero", sess.TunnelLostAt)
	}

	// Unknown id is a silent no-op.
	if err := st.SetSessionTunnelLost("s_missing", lost); err != nil {
		t.Fatalf("SetSessionTunnelLost(unknown): %v", err)
	}
}

// TestTunnelLostAtColumnMigration proves the C5 column exists after Open runs
// its migration: a basic create + get that touches tunnel_lost_at would fail at
// the SELECT if the column were absent. (openTestStore goes through Open, which
// runs ensureSessionTunnelLostAt.)
func TestTunnelLostAtColumnMigration(t *testing.T) {
	st := openTestStore(t)
	makeSession(t, st, "s_mig", "admin", statusReadySeed)
	if sess, found, err := st.GetSession("s_mig"); err != nil || !found || !sess.TunnelLostAt.IsZero() {
		t.Fatalf("GetSession after migration: found=%v err=%v lostAt=%v", found, err, sess)
	}
}
