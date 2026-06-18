package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/backhand/ecu/internal/store"
)

// Session status values, matching the API contract.
const (
	statusProvisioning = "provisioning"
	statusReady        = "ready"
	statusError        = "error"
	statusTerminated   = "terminated"
)

// createSessionRequest is the POST /sessions body. Both fields are optional.
// Restore is accepted (for C8) but not acted on in Component 2.
type createSessionRequest struct {
	Persistent bool    `json:"persistent"`
	Restore    *string `json:"restore"`
}

// createSessionResponse is the POST /sessions response.
//
// TunnelToken and TunnelURL are a DEV-ONLY testability seam: they are populated
// (and serialized) ONLY when the server was built WithExposeTunnelToken(true)
// (ECU_DEV_EXPOSE_TUNNEL_TOKEN=1) AND a token was actually generated (the
// non-dev provisioning path). In every other case they are empty and omitempty
// keeps them out of the JSON entirely, so production clients never see the
// tunnel secret.
type createSessionResponse struct {
	SessionID   string `json:"session_id"`
	Status      string `json:"status"`
	Persistent  bool   `json:"persistent"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	TunnelToken string `json:"tunnel_token,omitempty"`
	TunnelURL   string `json:"tunnel_url,omitempty"`
}

// getSessionResponse is the GET /sessions/{id} response. WatchURL is always
// null in Component 2 (filled by C9). Detail is optional and only set for error
// sessions.
type getSessionResponse struct {
	Status   string  `json:"status"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	WatchURL *string `json:"watch_url"`
	Detail   string  `json:"detail,omitempty"`
}

// handleCreateSession creates a new session record for the authenticated
// account.
//
// Two paths:
//
//   - Dev mode (ECU_DEV_TOOLSERVER set): the session is pointed at that tool
//     server and marked ready immediately, since there is no provisioning step.
//     No tunnel token is generated (the dev path reaches the tool server
//     directly, so a token would be unused and is left empty to avoid surprises).
//   - Otherwise (C3): the session is created as PROVISIONING with a fresh
//     per-session tunnel token. It becomes ready when an agent dials
//     /agent/connect and authenticates with that token. (This replaces the C2
//     skeleton behavior of recording an `error` session.)
//
// The persistent flag is accepted and stored but treated as ephemeral here;
// full persistence handling (authorization, snapshot/restore) is C8.
//
// The response includes tunnel_token/tunnel_url ONLY under the dev-only
// ECU_DEV_EXPOSE_TUNNEL_TOKEN seam (and only on the provisioning path, where a
// token exists); see createSessionResponse.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	account, _ := accountFromContext(r.Context())

	var req createSessionRequest
	// An empty body is valid (all fields optional); only reject malformed JSON.
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	id, err := store.NewSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}

	sess := &store.Session{
		ID:         id,
		Account:    account,
		Persistent: req.Persistent,
		Width:      defaultWidth,
		Height:     defaultHeight,
	}
	if s.devToolServer != "" {
		// Dev seam: skip provisioning, forward to the local tool server. No
		// tunnel token (direct path).
		sess.Status = statusReady
		sess.ToolEndpoint = s.devToolServer
	} else {
		// Production path: provision a session awaiting its reverse tunnel.
		token, err := store.NewTunnelToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not create session")
			return
		}
		sess.Status = statusProvisioning
		sess.ToolEndpoint = ""
		sess.TunnelToken = token
	}

	if err := s.store.CreateSession(sess); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist session")
		return
	}

	// Production path: kick off provisioning in the BACKGROUND and return
	// `provisioning` immediately (HTTP 200), matching the documented API
	// contract (the client polls GET /sessions/{id} until ready). The
	// background goroutine creates the instance, waits for the agent to
	// register, and tears the instance down on failure/timeout. It uses a
	// context derived from context.Background() (NOT the request context, which
	// ends when this handler returns). A nil provider means provisioning is not
	// configured (e.g. C2/C3 builds): leave the session provisioning as before.
	if s.devToolServer == "" && s.provider != nil {
		go s.provisionSession(sess.ID, sess.TunnelToken)
	}

	resp := createSessionResponse{
		SessionID:  sess.ID,
		Status:     sess.Status,
		Persistent: sess.Persistent,
		Width:      sess.Width,
		Height:     sess.Height,
	}
	// Dev-only testability seam: surface the tunnel token + ws URL so a test (or
	// a local agent) can connect. Only when explicitly enabled AND a token was
	// generated (the provisioning path).
	if s.exposeTunnelToken && sess.TunnelToken != "" {
		resp.TunnelToken = sess.TunnelToken
		resp.TunnelURL = "ws://" + s.listenAddr + agentConnectPath
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetSession returns a session's status and dimensions. Unknown ids yield
// 404. An error session may carry an optional detail field.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, found, err := s.store.GetSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}
	resp := getSessionResponse{
		Status:   sess.Status,
		Width:    sess.Width,
		Height:   sess.Height,
		WatchURL: nil, // filled by C9
	}
	if sess.Status == statusError {
		resp.Detail = "session provisioning failed; the instance (if any) has been torn down"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteSession marks a session terminated and returns
// {"status":"terminated"}. Unknown ids yield 404. Deleting an already-known
// (including already-terminated) session returns the terminated status again,
// making the operation idempotent-ish.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, found, err := s.store.GetSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}
	// Teardown = destroy the instance (these are disposable; C8 will snapshot
	// persistent ones instead). Best-effort and idempotent: DeleteInstance
	// tolerates an already-destroyed instance, so a repeated DELETE is safe and
	// a background provisioning waiter that also tears down does not conflict.
	// In dev mode (no provider / no instance id) this is a no-op and we just
	// mark terminated as before. We mark terminated FIRST so a concurrent
	// provisioning waiter observes the terminated state and stops without
	// resurrecting the session.
	if err := s.store.UpdateSessionStatus(id, statusTerminated); err != nil {
		writeError(w, http.StatusInternalServerError, "could not terminate session")
		return
	}
	if s.provider != nil && sess.InstanceID != "" {
		s.teardownInstance(id, sess.InstanceID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": statusTerminated})
}

// decodeOptionalJSON decodes JSON from body into v, treating an empty body as a
// valid no-op (leaving v at its zero value). Any non-empty malformed body
// returns an error.
func decodeOptionalJSON(body io.Reader, v any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty body
		}
		return err
	}
	return nil
}
