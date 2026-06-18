package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/backhand/ecu/internal/store"
)

// Session status values, matching the API contract. statusStopped (C8) is the
// extra, restorable state a PERSISTENT session enters when it is snapshotted +
// its instance destroyed (by DELETE or the cost-aware reaper): it holds no
// instance, only a stored snapshot of its saved desktop state, and can be
// reactivated via POST /sessions {restore:"<id>"}. It is non-terminal for the
// persistent cap (still counts) but is NOT "active" for the ephemeral
// ECU_MAX_SESSIONS cap and is excluded from the idle/orphan reaper sweep (no
// instance to leak); a separate cull pass ages it out to terminated.
const (
	statusProvisioning = "provisioning"
	statusReady        = "ready"
	statusError        = "error"
	statusTerminated   = "terminated"
	statusStopped      = "stopped"
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

// persistenceNotAuthorizedDetail is the exact 403 body for an unauthorized
// persistent / restore request. It is a settled wording (see the API contract):
// such a request is REJECTED, never silently downgraded to ephemeral.
const persistenceNotAuthorizedDetail = "persistence not authorized for this API key"

// handleCreateSession creates (or restores) a session for the authenticated
// account. After decoding the body and resolving the account + persistent
// capability it dispatches to one of THREE branches:
//
//   - Restore (req.Restore non-empty): re-activate a prior STOPPED persistent
//     session owned by this account, booting a fresh instance from its saved
//     snapshot. Requires the persistent capability. See handleRestoreSession.
//   - Persistent (req.Persistent): a new persistent session. Requires the
//     persistent capability (else 403, never a downgrade) and is bounded by the
//     ECU_MAX_PERSISTENT_SESSIONS cap (429). See handleNewSession.
//   - Ephemeral (otherwise): the original disposable-desktop path, bounded by
//     the ECU_MAX_SESSIONS active cap (429). See handleNewSession.
//
// Each branch keeps the dev-toolserver seam (ready immediately, no provider)
// and the dev-only tunnel-token exposure seam working.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	account, _ := accountFromContext(r.Context())
	persistentAllowed := persistentAllowedFromContext(r.Context())

	var req createSessionRequest
	// An empty body is valid (all fields optional); only reject malformed JSON.
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	switch {
	case req.Restore != nil && *req.Restore != "":
		s.handleRestoreSession(w, account, persistentAllowed, *req.Restore)
	case req.Persistent:
		s.handleNewSession(w, account, persistentAllowed, true)
	default:
		s.handleNewSession(w, account, persistentAllowed, false)
	}
}

// handleNewSession creates a brand-new ephemeral OR persistent session.
//
// Authorization + caps (enforced BEFORE generating an id, persisting, or
// touching the provider, so a rejected request never creates a row):
//
//   - Persistent: the API key MUST carry the persistent capability, else 403
//     with persistenceNotAuthorizedDetail (REJECT, never downgrade to
//     ephemeral). The ECU_MAX_PERSISTENT_SESSIONS cap counts every
//     non-terminated persistent session (provisioning + ready + stopped); at/over
//     it the request is 429. A persistent session is also ACTIVE, so it ALSO
//     consumes the ephemeral active cap; that's intentional (it holds a paid
//     instance). The persistent cap is checked FIRST so an over-persistent-cap
//     request gets the persistent-specific 429 message.
//   - Ephemeral: the original ECU_MAX_SESSIONS active-session cap (429).
//
// Both then create the row (dev seam: ready immediately; production: provisioning
// with a fresh tunnel token) and, on the production path, kick off provisioning
// in the background — identical to the pre-C8 flow.
func (s *Server) handleNewSession(w http.ResponseWriter, account string, persistentAllowed, persistent bool) {
	if persistent {
		if !persistentAllowed {
			writeError(w, http.StatusForbidden, persistenceNotAuthorizedDetail)
			return
		}
		// Persistent cap: count non-terminated persistent sessions (a stopped one
		// still counts until culled). 0 means unlimited.
		if s.maxPersistentSessions > 0 {
			n, err := s.store.CountNonTerminatedPersistentSessions()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if n >= s.maxPersistentSessions {
				writeError(w, http.StatusTooManyRequests,
					fmt.Sprintf("persistent session cap reached: %d persistent sessions (max %d)", n, s.maxPersistentSessions))
				return
			}
		}
	}

	// Enforce the global active-session cap. maxSessions==0 means unlimited. The
	// cap counts ACTIVE (provisioning+ready) sessions; error/terminated/stopped
	// rows do not count, so a reaped or stopped session frees an active slot.
	if s.maxSessions > 0 {
		n, err := s.store.CountActiveSessions()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if n >= s.maxSessions {
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("session cap reached: %d active sessions (max %d)", n, s.maxSessions))
			return
		}
	}

	id, err := store.NewSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}

	sess := &store.Session{
		ID:         id,
		Account:    account,
		Persistent: persistent,
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
	// contract (the client polls GET /sessions/{id} until ready). A new session
	// (ephemeral or persistent) cold-boots from the active boot image — the
	// snapshot-restore boot path is the restore branch, not here. A nil provider
	// means provisioning is not configured (dev/C2/C3): leave it provisioning.
	if s.devToolServer == "" && s.provider != nil {
		go s.provisionSession(sess.ID, sess.TunnelToken)
	}

	s.writeCreateResponse(w, sess)
}

// writeCreateResponse writes the POST /sessions success body for a created or
// restored session, including the dev-only tunnel_token/tunnel_url exposure
// seam (only when enabled AND a token exists, i.e. the production path).
func (s *Server) writeCreateResponse(w http.ResponseWriter, sess *store.Session) {
	resp := createSessionResponse{
		SessionID:  sess.ID,
		Status:     sess.Status,
		Persistent: sess.Persistent,
		Width:      sess.Width,
		Height:     sess.Height,
	}
	if s.exposeTunnelToken && sess.TunnelToken != "" {
		resp.TunnelToken = sess.TunnelToken
		resp.TunnelURL = "ws://" + s.listenAddr + agentConnectPath
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRestoreSession reactivates a prior STOPPED persistent session, booting a
// NEW instance from its saved snapshot and reusing the SAME session id.
//
// Authorization + validation (REJECT, never downgrade):
//
//   - The API key MUST carry the persistent capability, else 403 with
//     persistenceNotAuthorizedDetail. (Checked first: restore is a persistent
//     operation regardless of which session is named.)
//   - The named session must EXIST and be OWNED by this account, else 404
//     "unknown session". A not-owned session is reported as 404 (not 403) so one
//     account cannot probe for another account's session ids (no ownership leak).
//   - The session must be a PERSISTENT session in the 'stopped' state, else 409
//     "session is not a restorable stopped persistent session" (it exists and is
//     owned, but cannot be restored — e.g. it is ready, ephemeral, or already
//     terminated).
//
// On success it mints a FRESH tunnel token, resets the row to provisioning
// (clearing instance_id / tunnel_lost_at / stopped_at but KEEPING the snapshot,
// which is the saved state the next end replaces), and provisions a new instance
// booting from the snapshot ref (NOT ActiveBootImage). The response carries
// persistent:true. The dev seam reactivates to ready immediately with no
// provider, mirroring handleNewSession.
func (s *Server) handleRestoreSession(w http.ResponseWriter, account string, persistentAllowed bool, restoreID string) {
	if !persistentAllowed {
		writeError(w, http.StatusForbidden, persistenceNotAuthorizedDetail)
		return
	}

	sess, found, err := s.store.GetSession(restoreID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Not found OR not owned by this account -> 404 (do not reveal another
	// account's session by distinguishing the two cases).
	if !found || sess.Account != account {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}
	// Found and owned, but not a restorable stopped persistent session -> 409.
	if !sess.Persistent || sess.Status != statusStopped {
		writeError(w, http.StatusConflict, "session is not a restorable stopped persistent session")
		return
	}

	// The snapshot ref is the saved state to boot from. A stopped persistent
	// session should always carry one; guard defensively.
	snapshotRef := sess.SnapshotImage
	if snapshotRef == "" {
		writeError(w, http.StatusConflict, "session has no saved snapshot to restore from")
		return
	}

	if s.devToolServer != "" {
		// Dev seam: no provisioning. Reactivate the row (clears stopped_at, mints a
		// token we don't use), then point it at the dev tool server and mark ready
		// immediately — mirroring handleNewSession's dev path.
		token, _ := store.NewTunnelToken()
		if err := s.store.ReactivateSessionForRestore(restoreID, token); err != nil {
			writeError(w, http.StatusInternalServerError, "could not restore session")
			return
		}
		if err := s.store.UpdateSessionToolEndpoint(restoreID, s.devToolServer); err != nil {
			writeError(w, http.StatusInternalServerError, "could not restore session")
			return
		}
		if err := s.store.UpdateSessionStatus(restoreID, statusReady); err != nil {
			writeError(w, http.StatusInternalServerError, "could not restore session")
			return
		}
		fresh, _, _ := s.store.GetSession(restoreID)
		s.writeCreateResponse(w, fresh)
		return
	}

	// Production path: fresh tunnel token, reset the row to provisioning (keeping
	// the snapshot), and boot a new instance FROM THE SNAPSHOT.
	token, err := store.NewTunnelToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore session")
		return
	}
	if err := s.store.ReactivateSessionForRestore(restoreID, token); err != nil {
		writeError(w, http.StatusInternalServerError, "could not restore session")
		return
	}
	if s.provider != nil {
		// Boot from the saved snapshot ref (numeric image id -> hcloud's id path),
		// NOT ActiveBootImage. The snapshot is KEPT; the next end replaces it.
		go s.provisionSessionFromImage(restoreID, token, snapshotRef)
	}

	fresh, _, _ := s.store.GetSession(restoreID)
	if fresh == nil {
		// Should not happen (we just updated it); fall back to the in-memory view.
		fresh = sess
		fresh.Status = statusProvisioning
		fresh.TunnelToken = token
	}
	s.writeCreateResponse(w, fresh)
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

// handleDeleteSession tears a session down. Unknown ids yield 404. The shape of
// teardown depends on the session:
//
//   - Persistent with a live instance (and a provider): SNAPSHOT-AND-STOP — the
//     instance is snapshotted into a per-session image, then destroyed, and the
//     session is marked 'stopped' (restorable) carrying the new snapshot ref.
//     The response is {"status":"stopped"}, NOT "terminated": a stopped session
//     is restorable, so "terminated" would be misleading (and the API doc says
//     "Persistent → snapshotted and stopped"). If the SNAPSHOT FAILS we PREFER
//     PRESERVING STATE: return 500 and leave the instance + session exactly as
//     they were (do NOT destroy, do NOT mark stopped) so no saved work is lost.
//     A second DELETE on an already-stopped session does NOT re-snapshot — it is
//     idempotent and just returns the stopped status.
//   - Ephemeral, or persistent with no instance (dev mode / never provisioned):
//     the original behavior — mark terminated FIRST (so a racing provisioning
//     waiter observes it and backs off), then best-effort destroy any instance.
//     Response {"status":"terminated"}.
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

	// Idempotent-ish: a DELETE of an already-stopped persistent session must NOT
	// re-snapshot. It has no instance and already holds its saved state, so just
	// report stopped again.
	if sess.Persistent && sess.Status == statusStopped {
		writeJSON(w, http.StatusOK, map[string]string{"status": statusStopped})
		return
	}

	// Persistent end with a live instance: snapshot-and-stop (preserve state).
	if sess.Persistent && sess.InstanceID != "" && s.provider != nil {
		if err := s.snapshotAndStop(sess); err != nil {
			// Snapshot failed: prefer state. Leave the instance + session as-is and
			// report a server error so the client can retry; nothing was destroyed.
			log.Printf("ecu sessions: persistent DELETE of %s: snapshot failed, preserving state: %v", id, err)
			writeError(w, http.StatusInternalServerError, "could not snapshot session; state preserved, try again")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": statusStopped})
		return
	}

	// Ephemeral (or persistent with no instance): destroy. Mark terminated FIRST
	// so a concurrent provisioning waiter observes the terminal state and stops
	// without resurrecting the session; teardown is best-effort and idempotent.
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
