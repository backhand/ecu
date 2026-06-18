package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/provider/fake"
	"github.com/backhand/ecu/internal/store"
	"github.com/backhand/ecu/internal/tunnel"
)

// testClock is a mutable, concurrency-safe clock for driving the reaper's
// time-dependent rules deterministically (no real sleeping).
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// seedSession inserts a session row (status set after create so any state is
// reachable) carrying instanceID, and returns its real CreatedAt as recorded by
// the store. CreateSession forces CreatedAt==LastActivityAt==now and does NOT
// overwrite InstanceID, so the returned time is the anchor the test advances
// the injected clock relative to.
func seedSession(t *testing.T, st *store.Store, id, status, instanceID string) time.Time {
	t.Helper()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusProvisioning,
		Width: defaultWidth, Height: defaultHeight, InstanceID: instanceID,
	}); err != nil {
		t.Fatalf("CreateSession(%s): %v", id, err)
	}
	if status != statusProvisioning {
		if err := st.UpdateSessionStatus(id, status); err != nil {
			t.Fatalf("UpdateSessionStatus(%s,%s): %v", id, status, err)
		}
	}
	sess, found, err := st.GetSession(id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", id, found, err)
	}
	return sess.CreatedAt
}

// registerLiveTunnel registers a LIVE *tunnel.Tunnel (IsClosed()==false) for
// sessionID, kept alive for the whole test via t.Cleanup. It mirrors the proven
// ordering in internal/tunnel/tunnel_test.go: build the yamux SERVER over one
// end of a net.Pipe, then start RunClient (the yamux CLIENT) on the other end.
// No real tool server is needed — the reaper never opens a stream, it only
// reads IsClosed().
func registerLiveTunnel(t *testing.T, s *Server, sessionID string) {
	t.Helper()
	cpConn, agentConn := net.Pipe()
	tun, err := tunnel.NewServerTunnel(cpConn)
	if err != nil {
		t.Fatalf("NewServerTunnel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	clientDone := make(chan error, 1)
	go func() { clientDone <- tunnel.RunClient(ctx, agentConn, "127.0.0.1:0") }()
	t.Cleanup(func() { cancel(); tun.Close(); <-clientDone })

	if tun.IsClosed() {
		t.Fatalf("freshly built tunnel reports closed")
	}
	s.registry.register(sessionID, tun)
}

// assertReaped asserts the session is terminated and its instance was destroyed.
func assertReaped(t *testing.T, st *store.Store, prov *fake.Provider, id, instanceID string) {
	t.Helper()
	sess, found, err := st.GetSession(id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", id, found, err)
	}
	if sess.Status != statusTerminated {
		t.Fatalf("session %s status = %q, want terminated", id, sess.Status)
	}
	if !prov.Deleted(instanceID) {
		t.Fatalf("instance %s was NOT destroyed (leak!)", instanceID)
	}
}

// assertNotReaped asserts the session kept wantStatus and nothing was destroyed.
func assertNotReaped(t *testing.T, st *store.Store, prov *fake.Provider, id, wantStatus string) {
	t.Helper()
	sess, found, err := st.GetSession(id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", id, found, err)
	}
	if sess.Status != wantStatus {
		t.Fatalf("session %s status = %q, want %q (must not be reaped)", id, sess.Status, wantStatus)
	}
	if prov.DeleteCount() != 0 {
		t.Fatalf("DeleteCount = %d, want 0 (nothing should be torn down)", prov.DeleteCount())
	}
}

// TestReapIdle: a ready session with a LIVE tunnel is still reaped once
// last_activity_at is older than IdleTimeout (idle fires regardless of tunnel
// liveness). Lifetime is disabled so we isolate the idle rule.
func TestReapIdle(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{IdleTimeout: 30 * time.Minute, MaxLifetime: 0, OrphanGrace: time.Minute}),
	)

	created := seedSession(t, st, "s_idle", statusReady, "fake-1")
	registerLiveTunnel(t, s, "s_idle") // live tunnel: idle must still fire

	clk.set(created.Add(30*time.Minute + time.Second)) // just past IdleTimeout
	s.reapOnce(context.Background())

	assertReaped(t, st, prov, "s_idle", "fake-1")
}

// TestReapMaxLifetime: a session with recent activity but an old created_at is
// reaped by the lifetime rule. IdleTimeout is set larger than the elapsed
// inactivity so idle does NOT pre-empt; since lastActivityAt==createdAt for a
// fresh row, elapsed inactivity == elapsed lifetime, so IdleTimeout must exceed
// MaxLifetime+epsilon.
func TestReapMaxLifetime(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const maxLife = time.Hour
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{IdleTimeout: 24 * time.Hour, MaxLifetime: maxLife, OrphanGrace: time.Minute}),
	)

	created := seedSession(t, st, "s_life", statusReady, "fake-1")
	registerLiveTunnel(t, s, "s_life")

	clk.set(created.Add(maxLife + time.Second)) // just past MaxLifetime, well within IdleTimeout
	s.reapOnce(context.Background())

	assertReaped(t, st, prov, "s_life", "fake-1")
}

// TestReapOrphan: a provisioning session with NO tunnel is reaped once it has
// been tunnel-less for longer than ProvisionTimeout+OrphanGrace. Idle/lifetime
// disabled so only the orphan rule can fire.
func TestReapOrphan(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const provTimeout = time.Minute
	const orphanGrace = 2 * time.Minute
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: provTimeout}),
		WithReaperConfig(ReaperConfig{IdleTimeout: 0, MaxLifetime: 0, OrphanGrace: orphanGrace}),
	)

	created := seedSession(t, st, "s_orphan", statusProvisioning, "fake-1")
	// No tunnel registered: orphan candidate.

	clk.set(created.Add(provTimeout + orphanGrace + time.Second)) // past the window
	s.reapOnce(context.Background())

	assertReaped(t, st, prov, "s_orphan", "fake-1")
}

// TestNoReapFreshProvisioning: a provisioning session still inside the provision
// window (no tunnel yet) must NOT be reaped — agents need the full window to
// boot. Idle/lifetime disabled.
func TestNoReapFreshProvisioning(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const provTimeout = time.Minute
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: provTimeout}),
		WithReaperConfig(ReaperConfig{IdleTimeout: 0, MaxLifetime: 0, OrphanGrace: 2 * time.Minute}),
	)

	created := seedSession(t, st, "s_fresh", statusProvisioning, "fake-1")
	clk.set(created.Add(provTimeout / 2)) // well within the provision window
	s.reapOnce(context.Background())

	assertNotReaped(t, st, prov, "s_fresh", statusProvisioning)
}

// TestNoReapActiveReady: a ready session with a LIVE tunnel and recent activity,
// barely past creation and within all windows, must NOT be reaped.
func TestNoReapActiveReady(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		WithReaperConfig(ReaperConfig{IdleTimeout: 30 * time.Minute, MaxLifetime: 8 * time.Hour, OrphanGrace: 2 * time.Minute}),
	)

	created := seedSession(t, st, "s_active", statusReady, "fake-1")
	registerLiveTunnel(t, s, "s_active")

	clk.set(created.Add(5 * time.Second)) // barely advanced; within every window
	s.reapOnce(context.Background())

	assertNotReaped(t, st, prov, "s_active", statusReady)
}

// TestReapIdempotentTerminated covers two idempotency/concurrency properties:
//
//	(a) a terminated row is excluded from ListNonTerminalSessions, so reapOnce
//	    ignores it entirely (no teardown, status stays terminated);
//	(b) calling reapSession directly on a terminated row is a no-op (the fresh
//	    re-read + terminated check short-circuits) — modeling a reap that races
//	    a DELETE which already finished.
func TestReapIdempotentTerminated(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: time.Minute}),
		// Aggressive timeouts so, were the row non-terminal, it WOULD be reaped.
		WithReaperConfig(ReaperConfig{IdleTimeout: time.Second, MaxLifetime: time.Second, OrphanGrace: time.Second}),
	)

	created := seedSession(t, st, "s_term", statusTerminated, "fake-1")
	clk.set(created.Add(time.Hour)) // far past every window

	// (a) sweep ignores the terminated row.
	s.reapOnce(context.Background())
	if prov.DeleteCount() != 0 {
		t.Fatalf("terminated row was torn down by reapOnce: DeleteCount=%d", prov.DeleteCount())
	}
	if sess, _, _ := st.GetSession("s_term"); sess.Status != statusTerminated {
		t.Fatalf("terminated row status changed to %q", sess.Status)
	}

	// (b) direct reapSession on the terminated row is a no-op.
	sess, _, _ := st.GetSession("s_term")
	s.reapSession(*sess, "idle")
	if prov.DeleteCount() != 0 {
		t.Fatalf("reapSession on terminated row tore down an instance: DeleteCount=%d", prov.DeleteCount())
	}
	if sess2, _, _ := st.GetSession("s_term"); sess2.Status != statusTerminated {
		t.Fatalf("reapSession changed terminated status to %q", sess2.Status)
	}
}

// TestRestartBackstop proves reconcileTunnelLostAtBoot rebases the orphan clock
// to boot: a ready session with an EMPTY registry (post-restart) and a zero
// tunnel_lost_at is NOT instantly reaped — it gets a full
// ProvisionTimeout+OrphanGrace reconnect window measured from the reconcile,
// not from its (here, ancient) created_at. Past that window it IS reaped.
// Idle/lifetime disabled to isolate the orphan rule.
func TestRestartBackstop(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	clk := &testClock{}
	const provTimeout = time.Minute
	const orphanGrace = 2 * time.Minute
	window := provTimeout + orphanGrace
	s := NewServer(st, "",
		WithProvider(prov),
		WithClock(clk.now),
		WithProvisionConfig(ProvisionConfig{ProvisionTimeout: provTimeout}),
		WithReaperConfig(ReaperConfig{IdleTimeout: 0, MaxLifetime: 0, OrphanGrace: orphanGrace}),
	)

	// The session was created some time ago; simulate that by anchoring boot (T0)
	// AFTER its created_at. Without the boot rebase, the orphan rule (measured
	// from created_at) would fire immediately at T0.
	created := seedSession(t, st, "s_boot", statusReady, "fake-1")
	t0 := created.Add(time.Hour) // "boot" is an hour after the session was created
	clk.set(t0)

	// Registry is empty (no tunnel reconnected yet). Reconcile stamps
	// tunnel_lost_at=T0 for the tunnel-less session.
	s.reconcileTunnelLostAtBoot(context.Background())
	sess, _, _ := st.GetSession("s_boot")
	if sess.TunnelLostAt.IsZero() {
		t.Fatalf("boot reconcile did not stamp tunnel_lost_at")
	}

	// Advance to LESS than a full window past boot: must NOT reap (agent still
	// has a reconnect window). This is the crux — proves the clock is measured
	// from boot, not the ancient created_at.
	clk.set(t0.Add(window / 2))
	s.reapOnce(context.Background())
	assertNotReaped(t, st, prov, "s_boot", statusReady)

	// Advance past the full window from boot: now it IS an orphan.
	clk.set(t0.Add(window + time.Second))
	s.reapOnce(context.Background())
	assertReaped(t, st, prov, "s_boot", "fake-1")
}

// TestSessionCapEnforced exercises the global active-session cap through the
// HTTP handler in dev mode (sessions go ready immediately, no provider needed):
// two creates succeed, the third hits 429; after deleting one, a create
// succeeds again.
func TestSessionCapEnforced(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000", WithMaxSessions(2))

	id1 := createOK(t, srv)
	createOK(t, srv)

	// Third create: over the cap -> 429 with a JSON detail body.
	rec := doRequest(t, srv, http.MethodPost, "/sessions", "Bearer k_active")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third create status = %d, want 429", rec.Code)
	}
	assertJSONDetail(t, rec.Body.Bytes())

	// Delete one session: it becomes terminated and no longer counts active.
	if rec := doRequest(t, srv, http.MethodDelete, "/sessions/"+id1, "Bearer k_active"); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", rec.Code)
	}

	// A slot freed up: create succeeds again.
	createOK(t, srv)
}

// TestSessionCapUnlimited verifies WithMaxSessions(0) disables the cap.
func TestSessionCapUnlimited(t *testing.T) {
	st := newTestStore(t)
	srv := NewServer(st, "http://127.0.0.1:8000", WithMaxSessions(0))
	for i := 0; i < 5; i++ {
		createOK(t, srv) // never 429
	}
}

// createOK issues POST /sessions, asserts 200, and returns the new session id.
func createOK(t *testing.T, srv *Server) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/sessions", "Bearer k_active")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /sessions status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var cr createSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode create response: %v (%s)", err, rec.Body.String())
	}
	return cr.SessionID
}
