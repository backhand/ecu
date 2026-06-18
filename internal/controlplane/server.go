// Package controlplane implements the ECU control-plane HTTP API: API-key
// authentication, the session lifecycle (create / status / delete), and an
// opaque tool proxy that forwards action requests to each session's tool
// server.
//
// The proxy reaches tool servers exclusively through a SessionTransport seam
// (see transport.go). Component 2 wires a directTransport (plain HTTP to the
// stored endpoint); Component 3 swaps in a tunnelTransport without touching any
// handler, because the handlers depend only on the interface and never read the
// upstream endpoint themselves. This keeps the tool-server address off the
// client-facing path entirely.
//
// All error responses use the JSON shape {"detail": "..."} per the API
// contract.
package controlplane

import (
	"encoding/json"
	"net/http"

	"github.com/backhand/ecu/internal/store"
)

// fixed session dimensions for Component 2; real probing arrives with
// provisioning (C4).
const (
	defaultWidth  = 1280
	defaultHeight = 800
)

// Server holds the dependencies the HTTP handlers need: the persistence store,
// the SessionTransport used by the tool proxy, and the dev tool-server URL (the
// ECU_DEV_TOOLSERVER seam). It is constructed via NewServer and exposes its
// routes through Handler.
type Server struct {
	store     *store.Store
	transport SessionTransport

	// devToolServer is the ECU_DEV_TOOLSERVER value, empty in production. When
	// set, newly created sessions are pointed at it and marked ready
	// immediately so the control plane is exercisable end-to-end against a
	// local Component-1 tool server without real provisioning.
	devToolServer string
}

// NewServer builds a Server. The store is used for auth and session records;
// devToolServer is the ECU_DEV_TOOLSERVER seam (may be empty). The tool proxy's
// SessionTransport is a directTransport whose endpoint resolver reads
// tool_endpoint from the store — that lookup lives entirely behind the seam, so
// no handler ever touches the endpoint string.
func NewServer(st *store.Store, devToolServer string) *Server {
	s := &Server{
		store:         st,
		devToolServer: devToolServer,
	}
	s.transport = newDirectTransport(s.resolveEndpoint)
	return s
}

// resolveEndpoint is the EndpointResolver behind the directTransport: it maps a
// session id to its stored tool_endpoint. It returns ok=false for unknown,
// non-ready, or endpoint-less sessions. This is the ONLY place the endpoint is
// read for proxying, and it is on the transport side of the seam.
func (s *Server) resolveEndpoint(sessionID string) (string, bool) {
	sess, found, err := s.store.GetSession(sessionID)
	if err != nil || !found {
		return "", false
	}
	if sess.Status != statusReady || sess.ToolEndpoint == "" {
		return "", false
	}
	return sess.ToolEndpoint, true
}

// Handler returns the fully assembled http.Handler: a net/http.ServeMux using
// method+wildcard patterns (Go 1.22+) wrapped in the auth middleware so every
// route is authenticated.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{id}/{action}", s.handleAction)
	return s.authMiddleware(mux)
}

// writeJSON encodes v as JSON with the given status code and the proper
// content-type. Encoding errors are unlikely for our small payloads; if one
// occurs after the header is written there is nothing useful to do but stop.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a standardized JSON error body {"detail": msg} with the
// given status code. The message must never contain the upstream tool-server
// address.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"detail": msg})
}
