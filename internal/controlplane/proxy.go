package controlplane

import (
	"io"
	"net/http"
)

// allowedActions is the set of tool actions the proxy will forward, matching
// the API contract and the Component-1 tool server surface.
var allowedActions = map[string]bool{
	"click":      true,
	"move":       true,
	"type":       true,
	"key":        true,
	"scroll":     true,
	"exec":       true,
	"screenshot": true,
}

// handleAction proxies POST /sessions/{id}/{action} to the session's tool
// server and returns the tool server's status code and body verbatim.
//
// Flow and error mapping:
//   - unknown action            -> 400
//   - unknown session           -> 404
//   - session not in ready state -> 409
//   - transport cannot resolve   -> 404 (session/endpoint gone)
//   - upstream success/failure   -> status code + content-type + body copied through
//
// The upstream tool-server address is never exposed: the handler resolves an
// http.RoundTripper through the SessionTransport seam and builds the outbound
// request against the relative path "/"+action, so it never holds or emits the
// endpoint string. On each successful proxy the session's last_activity_at is
// updated for the future idle reaper (C5).
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.PathValue("action")

	if !allowedActions[action] {
		writeError(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}

	// Look up the session for status/existence checks (NOT for the endpoint).
	sess, found, err := s.store.GetSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}
	if sess.Status != statusReady {
		writeError(w, http.StatusConflict, "session is not ready")
		return
	}

	// Resolve the per-session RoundTripper through the seam. The endpoint
	// string stays entirely inside the transport.
	rt, ok := s.transport.RoundTripper(id)
	if !ok {
		// Endpoint disappeared between the status check and now.
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}

	// Build an outbound request to a relative path; the RoundTripper binds it
	// to the actual upstream. We use a placeholder host that never reaches the
	// client because the RoundTripper rewrites scheme/host before dialing.
	outURL := "http://tool-server/" + action
	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, outURL, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build upstream request")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		outReq.Header.Set("Content-Type", ct)
	}
	outReq.ContentLength = r.ContentLength

	resp, err := rt.RoundTrip(outReq)
	if err != nil {
		// Do not include the underlying error: it may contain the upstream
		// host:port. Return a generic 502.
		writeError(w, http.StatusBadGateway, "tool server unreachable")
		return
	}
	defer resp.Body.Close()

	// Record activity for the idle reaper (best-effort).
	_ = s.store.TouchSession(id)

	// Copy the upstream content-type and status, then stream the body verbatim.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
