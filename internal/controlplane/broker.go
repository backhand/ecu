package controlplane

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/backhand/ecu/internal/tunnel"
	"github.com/coder/websocket"
)

// handleAgentConnect is the C3 reverse-tunnel ingress: GET /agent/connect. An
// instance agent dials OUT to this endpoint over WebSocket, authenticates with
// its session-scoped tunnel token, and — once authenticated — the connection
// becomes a yamux mux the control plane uses to reach that session's tool
// server. No inbound port is ever opened on the instance.
//
// Auth is by per-session tunnel token, NOT API key (authMiddleware exempts this
// path). The token is accepted as "Authorization: Bearer <token>" or a ?token=
// query parameter (both supported; the bundled agent sends the Bearer header).
//
// Order of operations is security-critical:
//
//  1. Extract the presented token.
//  2. Resolve it to a session via the store (constant-time compare inside
//     SessionByTunnelToken). On miss/empty/terminated → 401 and RETURN WITHOUT
//     upgrading or registering anything.
//  3. ONLY THEN upgrade the WebSocket, raise its read limit (so large frames
//     like screenshots survive), build the yamux Server tunnel, register it,
//     and flip the session to ready.
//  4. Block until the tunnel dies or the request context is cancelled, then
//     deregister, close the tunnel, and flip the session out of ready
//     (provisioning) so subsequent actions get 409.
func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	token := agentToken(r)

	// Authenticate BEFORE any upgrade/registration. SessionByTunnelToken rejects
	// the empty token up front and uses a constant-time compare for the match.
	sess, found, err := s.store.SessionByTunnelToken(token)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		// Unknown/empty token: indistinguishable to the caller.
		http.Error(w, "invalid tunnel token", http.StatusUnauthorized)
		return
	}
	if sess.Status == statusTerminated {
		// A terminated session must not be revived by a reconnecting agent.
		http.Error(w, "session terminated", http.StatusUnauthorized)
		return
	}
	sessionID := sess.ID

	// Reject a second concurrent tunnel for the same session rather than
	// silently displacing the first. The simplest correct choice: if a live
	// tunnel is already registered, refuse the new one (the old one keeps
	// serving; the agent will back off and retry, by which point the stale one
	// will have been reaped if it was truly dead). This avoids a race where two
	// agents fight over one session id.
	if existing, ok := s.registry.lookup(sessionID); ok && !existing.IsClosed() {
		http.Error(w, "session already has an active tunnel", http.StatusConflict)
		return
	}

	// Auth passed. Upgrade the WebSocket. The instance host is always an
	// authorized origin (the agent is not a browser), so default origin checks
	// are fine; we accept the binary subprotocol implicitly via MessageBinary.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		// Accept already wrote an error response on failure.
		log.Printf("ecu broker: ws accept for session %s: %v", sessionID, err)
		return
	}
	// yamux frames can exceed coder/websocket's default 32 KiB read limit (a
	// single screenshot easily does); disable the limit so large frames are not
	// truncated and the tunnel does not break. MUST be set before NetConn wraps
	// the conn and must match the agent side.
	c.SetReadLimit(-1)

	// The NetConn context must span the WHOLE tunnel lifetime, never a
	// per-request context. We derive it from the request context so client
	// disconnects/server shutdown cancel it, and cancel it ourselves on cleanup
	// so websocket.NetConn tears down cleanly.
	connCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	netConn := websocket.NetConn(connCtx, c, websocket.MessageBinary)

	tun, err := tunnel.NewServerTunnel(netConn)
	if err != nil {
		log.Printf("ecu broker: build tunnel for session %s: %v", sessionID, err)
		_ = c.Close(websocket.StatusInternalError, "tunnel setup failed")
		return
	}

	// Register and flip to ready. register returns any prior entry; we re-checked
	// above, but if a stale (closed) one slipped in, close it.
	if prev, had := s.registry.register(sessionID, tun); had && prev != nil {
		_ = prev.Close()
	}
	if err := s.store.UpdateSessionStatus(sessionID, statusReady); err != nil {
		log.Printf("ecu broker: mark session %s ready: %v", sessionID, err)
	}
	// The agent is connected, so clear any prior tunnel-loss stamp: the
	// orphan/reconnect window the reaper measures must not count time the
	// session now has a live tunnel. Real wall-clock semantics here (a zero
	// time clears the column); the reaper alone uses the injected clock.
	if err := s.store.SetSessionTunnelLost(sessionID, time.Time{}); err != nil {
		log.Printf("ecu broker: clear session %s tunnel-loss stamp: %v", sessionID, err)
	}
	log.Printf("ecu broker: session %s tunnel established", sessionID)

	// Cleanup on exit: deregister and close the tunnel. Only reset the session
	// status if WE were still the registered tunnel (remove==true): if a newer
	// connection displaced us, it now owns the session's status and we must not
	// stomp its ready state. When we do own it, flip the session out of ready so
	// the proxy returns 409 for subsequent actions. We use provisioning (a
	// non-ready, non-terminal state) per the brief; a terminated session is left
	// as-is.
	defer func() {
		removed := s.registry.remove(sessionID, tun)
		_ = tun.Close()
		if removed {
			if cur, ok, _ := s.store.GetSession(sessionID); ok && cur.Status != statusTerminated {
				if err := s.store.UpdateSessionStatus(sessionID, statusProvisioning); err != nil {
					log.Printf("ecu broker: reset session %s status: %v", sessionID, err)
				}
				// Stamp the tunnel-loss instant so the reaper measures the
				// orphan/reconnect window from now (real wall-clock time), not
				// from the session's original created_at. If the agent
				// redials within ProvisionTimeout+OrphanGrace the register path
				// clears this again; otherwise the session is reaped as an
				// orphan and its instance destroyed.
				if err := s.store.SetSessionTunnelLost(sessionID, time.Now().UTC()); err != nil {
					log.Printf("ecu broker: stamp session %s tunnel-loss: %v", sessionID, err)
				}
			}
		}
		log.Printf("ecu broker: session %s tunnel closed", sessionID)
	}()

	// Block until the tunnel dies or the request context is cancelled.
	select {
	case <-tun.Wait():
	case <-connCtx.Done():
	}
}

// agentToken extracts the tunnel token from the request, accepting either an
// "Authorization: Bearer <token>" header (preferred; what the bundled agent
// sends) or a ?token= query parameter. The header takes precedence. An empty
// result is rejected downstream by SessionByTunnelToken.
func agentToken(r *http.Request) string {
	if tok, ok := bearerToken(r.Header.Get("Authorization")); ok {
		return tok
	}
	return r.URL.Query().Get("token")
}
