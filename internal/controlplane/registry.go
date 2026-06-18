package controlplane

import (
	"sync"

	"github.com/backhand/ecu/internal/tunnel"
)

// tunnelRegistry maps a session id to its live reverse tunnel. The broker
// (handleAgentConnect) registers a tunnel when an agent connects and
// authenticates, and removes it when the tunnel dies. The tunnelTransport reads
// it on every proxied action to find the RoundTripper for a session.
//
// It is safe for concurrent use; all access is guarded by mu.
type tunnelRegistry struct {
	mu sync.Mutex
	m  map[string]*tunnel.Tunnel
}

// newTunnelRegistry returns an empty registry ready for use.
func newTunnelRegistry() *tunnelRegistry {
	return &tunnelRegistry{m: make(map[string]*tunnel.Tunnel)}
}

// register binds tun to sessionID. It returns the previously registered tunnel
// (if any) so the caller can decide how to handle a double-registration; the
// new tunnel always replaces the old in the map. Callers that reject duplicates
// instead should consult lookup first.
func (r *tunnelRegistry) register(sessionID string, tun *tunnel.Tunnel) (prev *tunnel.Tunnel, hadPrev bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, hadPrev = r.m[sessionID]
	r.m[sessionID] = tun
	return prev, hadPrev
}

// lookup returns the live tunnel for sessionID, if one is registered.
func (r *tunnelRegistry) lookup(sessionID string) (*tunnel.Tunnel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tun, ok := r.m[sessionID]
	return tun, ok
}

// remove deletes the registry entry for sessionID, but only if the currently
// registered tunnel is exactly tun. This prevents a slow-exiting old connection
// from deleting a freshly-registered replacement's entry (the
// register-then-defer-remove race). Passing a nil tun removes unconditionally.
// It reports whether an entry was actually deleted, which the broker uses to
// decide whether it owns the session's status (and may reset it) on cleanup.
func (r *tunnelRegistry) remove(sessionID string, tun *tunnel.Tunnel) (removed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[sessionID]; ok && (tun == nil || cur == tun) {
		delete(r.m, sessionID)
		return true
	}
	return false
}
