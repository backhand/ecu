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

// reapOnce performs a single reaping sweep: it lists the non-terminal sessions
// and, for each, applies the reaping rules in priority order, reaping the first
// match. It is the unit the tests drive directly (advancing the injected clock
// and calling reapOnce), so it must be side-effect-deterministic given the
// store + registry + clock.
//
// Rule priority (FIRST match wins):
//
//  1. lifetime — created_at older than MaxLifetime (hard ceiling).
//  2. idle     — last_activity_at older than IdleTimeout.
//  3. orphan   — no live tunnel for longer than the orphan/reconnect window,
//     measured from max(created_at, tunnel_lost_at).
//
// A disabled timeout (0) simply never matches its rule. Idle/lifetime fire
// regardless of tunnel liveness (a wedged-but-connected session is still
// reaped); orphan fires ONLY when there is no live tunnel.
func (s *Server) reapOnce(ctx context.Context) {
	sessions, err := s.store.ListNonTerminalSessions()
	if err != nil {
		log.Printf("ecu reaper: list sessions: %v", err)
		return
	}
	// Orphan window: a tunnel-less session gets the full provisioning window
	// PLUS the reconnect grace before it is considered leaked.
	provisionWindow := s.provisionCfg.ProvisionTimeout + s.reaperCfg.OrphanGrace
	now := s.now()

	for _, sess := range sessions {
		reason := ""
		switch {
		case s.reaperCfg.MaxLifetime > 0 && now.Sub(sess.CreatedAt) > s.reaperCfg.MaxLifetime:
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

// reapSession terminates one doomed session and tears down its backing
// instance. It is idempotent and concurrency-safe against a parallel DELETE:
//
//   - It RE-READS the session fresh and does NOTHING if it is gone or already
//     terminated (a concurrent DELETE finished first — no double-teardown, no
//     duplicate log line).
//   - It marks the session terminated BEFORE teardown so a racing
//     provisioning waiter / second reap observes the terminal state and backs
//     off; instance protection comes first, so teardown proceeds even if the
//     status write errors.
//   - teardownInstance is itself idempotent (DeleteInstance tolerates an
//     already-/concurrently-destroyed instance), so a double call is harmless.
func (s *Server) reapSession(sess store.Session, reason string) {
	cur, found, err := s.store.GetSession(sess.ID)
	if err != nil {
		log.Printf("ecu reaper: re-read session %s: %v", sess.ID, err)
		return
	}
	if !found || cur.Status == statusTerminated {
		// A concurrent DELETE (or a prior reap) already finished. Nothing to do.
		return
	}

	// Log exactly once per real reap, right after the terminated check.
	log.Printf("ecu reaper: reaping session %s (reason=%s, instance=%s)", sess.ID, reason, cur.InstanceID)

	// Mark terminal FIRST (so racing actors observe it), then destroy the
	// instance. Proceed to teardown even if the status write fails — protecting
	// the paid instance is the priority.
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
