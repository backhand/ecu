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
type createSessionResponse struct {
	SessionID  string `json:"session_id"`
	Status     string `json:"status"`
	Persistent bool   `json:"persistent"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
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
// In dev mode (ECU_DEV_TOOLSERVER set) the session is pointed at that tool
// server and marked ready immediately, since there is no provisioning step —
// this is the path that must work end-to-end. Without the dev seam, the session
// is created in the error state and returned with HTTP 200 (so a subsequent GET
// works); real provisioning arrives in C4.
//
// The persistent flag is accepted and stored but treated as ephemeral here;
// full persistence handling (authorization, snapshot/restore) is C8.
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
		// Dev seam: skip provisioning, forward to the local tool server.
		sess.Status = statusReady
		sess.ToolEndpoint = s.devToolServer
	} else {
		// No provisioning yet: record an error session so GET still works.
		sess.Status = statusError
		sess.ToolEndpoint = ""
	}

	if err := s.store.CreateSession(sess); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist session")
		return
	}

	writeJSON(w, http.StatusOK, createSessionResponse{
		SessionID:  sess.ID,
		Status:     sess.Status,
		Persistent: sess.Persistent,
		Width:      sess.Width,
		Height:     sess.Height,
	})
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
		resp.Detail = "session provisioning is not available in this build"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteSession marks a session terminated and returns
// {"status":"terminated"}. Unknown ids yield 404. Deleting an already-known
// (including already-terminated) session returns the terminated status again,
// making the operation idempotent-ish.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, found, err := s.store.GetSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "unknown session")
		return
	}
	if err := s.store.UpdateSessionStatus(id, statusTerminated); err != nil {
		writeError(w, http.StatusInternalServerError, "could not terminate session")
		return
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
