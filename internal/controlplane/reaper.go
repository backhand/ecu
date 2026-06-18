package controlplane

import (
	"context"
	"log"
	"time"

	"github.com/backhand/ecu/internal/store"
)

// defaultOrphanGrace is the slack added to ProvisionTimeout to form the
// orphan/reconnect window: a session with NO live tunnel is only reaped as an
// "orphan" once it has been without a tunnel for ProvisionTimeout+OrphanGrace.
// This covers cold-boot provisioning (the agent needs the full provision
// window to come up) PLUS a transient reconnect after a tunnel blip or a
// control-plane restart, so we never destroy a paid instance whose agent is
// merely a few seconds from (re)connecting. It is deliberately NOT configurable
// via an env var — it is an internal safety margin, not an operational knob.
const defaultOrphanGrace = 2 * time.Minute

// ReaperConfig carries the session reaper's timeouts and tick cadence. It is
// supplied via WithReaperConfig (derived from the loaded config in cmd/ecu).
//
// The zero value is safe: IdleTimeout/MaxLifetime of 0 DISABLE those rules
// (never reap on idle / lifetime), ReapInterval<=0 falls back to a default
// cadence, and OrphanGrace of 0 still leaves the orphan window at
// ProvisionTimeout. The orphan rule is always active — it is the backstop that
// protects against leaked instances after a crash/restart.
type ReaperConfig struct {
	// IdleTimeout reaps a session whose last_activity_at is older than this.
	// 0 disables idle reaping.
	IdleTimeout time.Duration

	// MaxLifetime reaps a session whose created_at is older than this,
	// regardless of activity. 0 disables lifetime reaping.
	MaxLifetime time.Duration

	// ReapInterval is the base sweep cadence. RunReaper clamps it DOWN so it
	// never exceeds the smallest positive timeout (so a short idle/lifetime is
	// not overshot). <=0 falls back to one minute.
	ReapInterval time.Duration

	// OrphanGrace is added to the provider's ProvisionTimeout to form the
	// orphan/reconnect window (see defaultOrphanGrace).
	OrphanGrace time.Duration

	// PersistentMaxLifetime is the hard lifetime ceiling for ACTIVE PERSISTENT
	// sessions, used INSTEAD of MaxLifetime for them (C8). It is deliberately
	// LONGER (a persistent session holds saved work, not a disposable desktop):
	// when it ages out the reaper snapshots-and-stops the session rather than
	// destroying it. 0 disables persistent-lifetime reaping. The idle + orphan
	// triggers stay SHARED across ephemeral and persistent.
	PersistentMaxLifetime time.Duration

	// PersistentMaxAge bounds how long a STOPPED persistent session's saved
	// snapshot is retained, measured from StoppedAt (C8). Past it the reaper's
	// cull pass deletes the snapshot and marks the session terminated (freeing a
	// persistent-cap slot). 0 disables stopped-session culling.
	PersistentMaxAge time.Duration
}

// RunReaper runs the reaping loop until ctx is cancelled. It is BLOCKING; main
// runs it in a goroutine alongside the HTTP server and cancels ctx on shutdown.
//
// On entry it FIRST runs reconcileTunnelLostAtBoot so the orphan/reconnect
// window for any session that has no live tunnel at startup is measured from
// boot (not from a possibly-ancient created_at) — this is the restart backstop
// (see reconcileTunnelLostAtBoot). It then ticks on a clamped interval and
// sweeps once per tick.
func (s *Server) RunReaper(ctx context.Context) {
	// Restart backstop: stamp tunnel_lost_at=now for tunnel-less sessions so the
	// orphan clock starts at boot, giving agents a full reconnect window.
	s.reconcileTunnelLostAtBoot(ctx)

	interval := s.reaperCfg.ReapInterval
	if interval <= 0 {
		interval = time.Minute
	}
	// Clamp DOWN to the smallest POSITIVE timeout so a short idle/lifetime is
	// detected promptly rather than overshot by a long interval. Disabled
	// (zero) timeouts never tighten the interval.
	for _, d := range []time.Duration{s.reaperCfg.IdleTimeout, s.reaperCfg.MaxLifetime} {
		if d > 0 && d < interval {
			interval = d
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapOnce(ctx)
		}
	}
}

// reapOnce performs a single reaping sweep. It runs TWO passes, both driven
// directly by tests (advance the injected clock, call reapOnce), so each must be
// side-effect-deterministic given the store + registry + clock:
//
//  1. The active-session sweep over ListNonTerminalSessions (provisioning +
//     ready). For each, the FIRST matching rule wins:
//     a. lifetime — created_at older than the lifetime ceiling. PERSISTENT
//     sessions use the LONGER PersistentMaxLifetime; ephemeral use
//     MaxLifetime. (A reaped persistent session is snapshot-and-stopped, not
//     destroyed — see reapSession.)
//     b. idle     — last_activity_at older than IdleTimeout (shared).
//     c. orphan   — no live tunnel for longer than the orphan/reconnect window,
//     measured from max(created_at, tunnel_lost_at) (shared).
//     A disabled timeout (0) never matches its rule. Idle/lifetime fire
//     regardless of tunnel liveness; orphan fires ONLY when there is no live
//     tunnel. STOPPED sessions are not in this list (status='stopped' ∉
//     provisioning,ready), so they are never idle/orphan-reaped — they have no
//     instance to leak.
//  2. The cull pass over ListStoppedPersistentSessions: a stopped persistent
//     session whose StoppedAt is older than PersistentMaxAge is culled (snapshot
//     deleted, marked terminated, freeing a persistent-cap slot).
func (s *Server) reapOnce(ctx context.Context) {
	now := s.now()

	sessions, err := s.store.ListNonTerminalSessions()
	if err != nil {
		log.Printf("ecu reaper: list sessions: %v", err)
	} else {
		// Orphan window: a tunnel-less session gets the full provisioning window
		// PLUS the reconnect grace before it is considered leaked.
		provisionWindow := s.provisionCfg.ProvisionTimeout + s.reaperCfg.OrphanGrace
		for _, sess := range sessions {
			// Lifetime ceiling depends on the session kind: persistent sessions get
			// the longer PersistentMaxLifetime, ephemeral the standard MaxLifetime.
			lifetime := s.reaperCfg.MaxLifetime
			if sess.Persistent {
				lifetime = s.reaperCfg.PersistentMaxLifetime
			}
			reason := ""
			switch {
			case lifetime > 0 && now.Sub(sess.CreatedAt) > lifetime:
				reason = "lifetime"
			case s.reaperCfg.IdleTimeout > 0 && now.Sub(sess.LastActivityAt) > s.reaperCfg.IdleTimeout:
				reason = "idle"
			default:
				// Orphan: only a candidate when there is NO live tunnel.
				if tun, ok := s.registry.lookup(sess.ID); !ok || tun.IsClosed() {
					orphanSince := sess.CreatedAt
					if sess.TunnelLostAt.After(orphanSince) {
						orphanSince = sess.TunnelLostAt
					}
					if now.Sub(orphanSince) > provisionWindow {
						reason = "orphan"
					}
				}
			}
			if reason == "" {
				continue
			}
			s.reapSession(sess, reason)
		}
	}

	s.cullStoppedOnce(now)
}

// cullStoppedOnce is reapOnce's second pass: it culls every STOPPED persistent
// session whose saved snapshot has aged out (StoppedAt older than
// PersistentMaxAge). PersistentMaxAge<=0 disables culling. Each cull deletes the
// snapshot and marks the session terminated (freeing a persistent-cap slot); it
// is idempotent and retry-safe (see cullStoppedSession).
func (s *Server) cullStoppedOnce(now time.Time) {
	if s.reaperCfg.PersistentMaxAge <= 0 {
		return // culling disabled
	}
	stopped, err := s.store.ListStoppedPersistentSessions()
	if err != nil {
		log.Printf("ecu reaper: list stopped persistent sessions: %v", err)
		return
	}
	for _, sess := range stopped {
		// Measure age from StoppedAt (created_at is wrong for a long-lived session
		// only recently stopped). A zero StoppedAt (shouldn't happen for a stopped
		// row) is treated as not-yet-cullable rather than instantly culled.
		if sess.StoppedAt.IsZero() {
			continue
		}
		if now.Sub(sess.StoppedAt) > s.reaperCfg.PersistentMaxAge {
			s.cullStoppedSession(sess)
		}
	}
}

// reapSession reaps one doomed ACTIVE session. It is idempotent and
// concurrency-safe against a parallel DELETE: it RE-READS the session fresh and
// does NOTHING if it is gone, already terminated, OR already stopped (a
// concurrent DELETE / reap finished first — no double work, no duplicate log).
//
// The reap action depends on the session kind:
//
//   - EPHEMERAL → destroy: mark terminated BEFORE teardown so a racing
//     provisioning waiter / second reap observes the terminal state and backs
//     off; instance protection comes first, so teardown proceeds even if the
//     status write errors. teardownInstance is idempotent.
//   - PERSISTENT → SNAPSHOT-AND-STOP (a reaped persistent session is an auto-end,
//     not a destroy): snapshot the instance, then destroy it and mark the session
//     'stopped' carrying the snapshot (snapshotAndStop). If the snapshot FAILS we
//     PRESERVE STATE — log loudly and RETURN WITHOUT terminating or destroying,
//     so the next sweep retries and no saved work is lost. The state transition
//     (to 'stopped') happens only AFTER a successful snapshot, mirroring the
//     ephemeral terminate-before-teardown invariant. A persistent session with no
//     instance (shouldn't happen for an active one) is just marked stopped.
func (s *Server) reapSession(sess store.Session, reason string) {
	cur, found, err := s.store.GetSession(sess.ID)
	if err != nil {
		log.Printf("ecu reaper: re-read session %s: %v", sess.ID, err)
		return
	}
	if !found || cur.Status == statusTerminated || cur.Status == statusStopped {
		// A concurrent DELETE (or a prior reap) already finished. Nothing to do.
		return
	}

	// Log exactly once per real reap, right after the short-circuit checks.
	log.Printf("ecu reaper: reaping session %s (reason=%s, persistent=%v, instance=%s)", sess.ID, reason, cur.Persistent, cur.InstanceID)

	if cur.Persistent {
		if cur.InstanceID == "" || s.provider == nil {
			// No instance to snapshot (e.g. a dev-mode persistent session, or one
			// that never got an instance): there is nothing to preserve via a
			// snapshot, so just stop it (no snapshot ref). It then ages out via the
			// cull pass like any other stopped session.
			if err := s.store.MarkSessionStopped(cur.ID, ""); err != nil {
				log.Printf("ecu reaper: mark persistent session %s stopped (no instance): %v", cur.ID, err)
			}
			return
		}
		if err := s.snapshotAndStop(cur); err != nil {
			// PRESERVE STATE: leave the instance running and the session active so
			// the next sweep retries. Do NOT destroy or terminate.
			log.Printf("ecu reaper: session %s: snapshot-and-stop FAILED, preserving state (instance %s left running, will retry): %v", cur.ID, cur.InstanceID, err)
		}
		return
	}

	// Ephemeral: mark terminal FIRST (so racing actors observe it), then destroy
	// the instance. Proceed to teardown even if the status write fails —
	// protecting the paid instance is the priority.
	if err := s.store.UpdateSessionStatus(sess.ID, statusTerminated); err != nil {
		log.Printf("ecu reaper: mark session %s terminated: %v", sess.ID, err)
	}
	s.teardownInstance(sess.ID, cur.InstanceID)
}

// reconcileTunnelLostAtBoot is the restart backstop. On startup the registry is
// empty (no tunnels have reconnected yet), so EVERY non-terminal session
// momentarily looks tunnel-less. If the orphan rule measured its window from
// created_at, a restart of a long-lived control plane would instantly reap
// healthy sessions whose agents simply haven't reconnected in the few seconds
// since boot. To prevent that, we stamp tunnel_lost_at=now for any non-terminal
// session that has NO live tunnel and NO existing loss stamp, which restarts
// the orphan/reconnect clock at boot. Agents then get a full
// ProvisionTimeout+OrphanGrace window to redial before being treated as
// orphans.
//
// Why this can't nuke healthy sessions:
//   - It only WRITES tunnel_lost_at; it never terminates or tears anything down.
//   - Sessions that already carry a non-zero tunnel_lost_at keep it (we don't
//     reset an in-flight reconnect window).
//   - The idle and max-lifetime rules are unaffected: they read the PERSISTED
//     created_at / last_activity_at, which a restart cannot reset, so they
//     remain the hard backstops. Only the orphan fast-path is rebased to boot.
func (s *Server) reconcileTunnelLostAtBoot(ctx context.Context) {
	sessions, err := s.store.ListNonTerminalSessions()
	if err != nil {
		log.Printf("ecu reaper: boot reconcile: list sessions: %v", err)
		return
	}
	now := s.now()
	stamped := 0
	for _, sess := range sessions {
		if sess.TunnelLostAt.IsZero() {
			if tun, ok := s.registry.lookup(sess.ID); !ok || tun.IsClosed() {
				if err := s.store.SetSessionTunnelLost(sess.ID, now); err != nil {
					log.Printf("ecu reaper: boot reconcile: stamp session %s: %v", sess.ID, err)
					continue
				}
				stamped++
			}
		}
	}
	if stamped > 0 {
		log.Printf("ecu reaper: boot reconcile: stamped tunnel-loss on %d tunnel-less session(s); orphan window starts now", stamped)
	}
}
