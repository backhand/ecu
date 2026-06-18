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

// statusStoppedSeed mirrors the controlplane 'stopped' status literal for the
// store tests (the store does not import the controlplane constants).
const statusStoppedSeed = "stopped"

// makePersistentSession inserts a PERSISTENT session row with the given id and
// status, returning the created row. Mirrors makeSession but sets Persistent.
func makePersistentSession(t *testing.T, st *Store, id, account, status string) *Session {
	t.Helper()
	if err := st.CreateSession(&Session{
		ID: id, Account: account, Status: statusProvisioningSeed,
		Persistent: true, Width: 1280, Height: 800,
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

// TestPersistColumnsMigration proves the C8 columns (snapshot_image, stopped_at)
// exist after Open: a create + get that scans them would fail at the SELECT if
// either column were absent. (openTestStore goes through Open, which runs
// ensureSessionSnapshotImage + ensureSessionStoppedAt.)
func TestPersistColumnsMigration(t *testing.T) {
	st := openTestStore(t)
	makeSession(t, st, "s_pmig", "admin", statusReadySeed)
	sess, found, err := st.GetSession("s_pmig")
	if err != nil || !found {
		t.Fatalf("GetSession after migration: found=%v err=%v", found, err)
	}
	if sess.SnapshotImage != "" || !sess.StoppedAt.IsZero() {
		t.Fatalf("fresh session SnapshotImage=%q StoppedAt=%v, want empty/zero", sess.SnapshotImage, sess.StoppedAt)
	}
}

// TestMarkSessionStoppedRoundTrip verifies MarkSessionStopped writes status,
// snapshot_image, and stopped_at atomically and they read back.
func TestMarkSessionStoppedRoundTrip(t *testing.T) {
	st := openTestStore(t)
	makePersistentSession(t, st, "s_stop", "admin", statusReadySeed)

	before := time.Now().UTC()
	if err := st.MarkSessionStopped("s_stop", "fake-image-ecu-persist-s_stop"); err != nil {
		t.Fatalf("MarkSessionStopped: %v", err)
	}
	after := time.Now().UTC()

	sess, _, _ := st.GetSession("s_stop")
	if sess.Status != statusStoppedSeed {
		t.Fatalf("status = %q, want stopped", sess.Status)
	}
	if sess.SnapshotImage != "fake-image-ecu-persist-s_stop" {
		t.Fatalf("snapshot_image = %q, want the snapshot ref", sess.SnapshotImage)
	}
	if sess.StoppedAt.Before(before) || sess.StoppedAt.After(after) {
		t.Fatalf("stopped_at = %v, want within [%v,%v]", sess.StoppedAt, before, after)
	}

	// Unknown id is a silent no-op.
	if err := st.MarkSessionStopped("s_missing", "x"); err != nil {
		t.Fatalf("MarkSessionStopped(unknown): %v", err)
	}
}

// TestCountNonTerminatedPersistentSessions verifies the persistent-cap count:
// every non-terminated persistent session counts (provisioning + ready +
// stopped); only terminated frees a slot, and ephemeral sessions never count.
func TestCountNonTerminatedPersistentSessions(t *testing.T) {
	st := openTestStore(t)

	if n, err := st.CountNonTerminatedPersistentSessions(); err != nil || n != 0 {
		t.Fatalf("empty store: count = %d, err = %v, want 0", n, err)
	}

	makePersistentSession(t, st, "p_prov", "admin", statusProvisioningSeed)
	makePersistentSession(t, st, "p_ready", "admin", statusReadySeed)
	makePersistentSession(t, st, "p_stopped", "admin", statusStoppedSeed)
	makePersistentSession(t, st, "p_term", "admin", statusTerminatedSeed) // does NOT count
	makeSession(t, st, "e_ready", "admin", statusReadySeed)               // ephemeral, does NOT count

	n, err := st.CountNonTerminatedPersistentSessions()
	if err != nil {
		t.Fatalf("CountNonTerminatedPersistentSessions: %v", err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3 (provisioning + ready + stopped persistent; not terminated, not ephemeral)", n)
	}
}

// TestListStoppedPersistentSessions verifies the cull-candidate list contains
// only stopped persistent sessions and round-trips their snapshot ref +
// stopped_at, ordered by stopped_at.
func TestListStoppedPersistentSessions(t *testing.T) {
	st := openTestStore(t)

	// One stopped persistent (the candidate) with a snapshot ref.
	makePersistentSession(t, st, "p_stopped", "admin", statusReadySeed)
	if err := st.MarkSessionStopped("p_stopped", "fake-image-ecu-persist-p_stopped"); err != nil {
		t.Fatalf("MarkSessionStopped: %v", err)
	}
	// Non-candidates: a ready persistent, a terminated persistent, an ephemeral
	// session that is somehow 'stopped' (shouldn't occur, but the WHERE must
	// require persistent=1).
	makePersistentSession(t, st, "p_ready", "admin", statusReadySeed)
	makePersistentSession(t, st, "p_term", "admin", statusTerminatedSeed)
	makeSession(t, st, "e_stopped", "admin", statusStoppedSeed)

	got, err := st.ListStoppedPersistentSessions()
	if err != nil {
		t.Fatalf("ListStoppedPersistentSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d stopped persistent sessions, want 1: %+v", len(got), got)
	}
	if got[0].ID != "p_stopped" {
		t.Fatalf("stopped list has %q, want p_stopped", got[0].ID)
	}
	if got[0].SnapshotImage != "fake-image-ecu-persist-p_stopped" {
		t.Fatalf("snapshot ref = %q, want the stored ref", got[0].SnapshotImage)
	}
	if got[0].StoppedAt.IsZero() {
		t.Fatalf("stopped_at is zero, want the stamped instant")
	}
}

// TestReactivateSessionForRestore verifies the restore reset: status back to
// provisioning, fresh tunnel token, instance_id / tunnel_lost_at / stopped_at
// cleared, and snapshot_image PRESERVED (it is the saved state to boot from).
func TestReactivateSessionForRestore(t *testing.T) {
	st := openTestStore(t)
	makePersistentSession(t, st, "p_restore", "admin", statusReadySeed)
	// Give it an instance id + a lost stamp, then stop it with a snapshot.
	if err := st.UpdateSessionInstanceID("p_restore", "fake-7"); err != nil {
		t.Fatalf("UpdateSessionInstanceID: %v", err)
	}
	if err := st.SetSessionTunnelLost("p_restore", time.Now().UTC()); err != nil {
		t.Fatalf("SetSessionTunnelLost: %v", err)
	}
	if err := st.MarkSessionStopped("p_restore", "fake-image-ecu-persist-p_restore"); err != nil {
		t.Fatalf("MarkSessionStopped: %v", err)
	}

	if err := st.ReactivateSessionForRestore("p_restore", "t_newtoken"); err != nil {
		t.Fatalf("ReactivateSessionForRestore: %v", err)
	}
	sess, _, _ := st.GetSession("p_restore")
	if sess.Status != statusProvisioningSeed {
		t.Fatalf("status = %q, want provisioning", sess.Status)
	}
	if sess.TunnelToken != "t_newtoken" {
		t.Fatalf("tunnel_token = %q, want t_newtoken", sess.TunnelToken)
	}
	if sess.InstanceID != "" {
		t.Fatalf("instance_id = %q, want cleared", sess.InstanceID)
	}
	if !sess.TunnelLostAt.IsZero() {
		t.Fatalf("tunnel_lost_at = %v, want cleared", sess.TunnelLostAt)
	}
	if !sess.StoppedAt.IsZero() {
		t.Fatalf("stopped_at = %v, want cleared", sess.StoppedAt)
	}
	// Snapshot ref must be PRESERVED — it is the saved state the restore boots from.
	if sess.SnapshotImage != "fake-image-ecu-persist-p_restore" {
		t.Fatalf("snapshot_image = %q, want preserved across restore", sess.SnapshotImage)
	}
	if !sess.Persistent {
		t.Fatalf("session lost its persistent flag across restore")
	}
}
