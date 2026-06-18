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
// the SessionTransport used by the tool proxy, the dev tool-server URL (the
// ECU_DEV_TOOLSERVER seam), and the C3 reverse-tunnel broker state. It is
// constructed via NewServer and exposes its routes through Handler.
type Server struct {
	store     *store.Store
	transport SessionTransport

	// registry holds live reverse tunnels keyed by session id. The broker
	// (handleAgentConnect) populates it; the tunnelTransport reads it.
	registry *tunnelRegistry

	// devToolServer is the ECU_DEV_TOOLSERVER value, empty in production. When
	// set, newly created sessions are pointed at it and marked ready
	// immediately so the control plane is exercisable end-to-end against a
	// local Component-1 tool server without real provisioning.
	devToolServer string

	// exposeTunnelToken, when true, makes POST /sessions additionally return the
	// session's tunnel_token and a tunnel_url. This is a DEV-ONLY testability
	// seam (ECU_DEV_EXPOSE_TUNNEL_TOKEN=1); in production these secrets are
	// never returned to API clients.
	exposeTunnelToken bool

	// listenAddr is the control-plane listen address (ECU_LISTEN), used only to
	// build the ws:// tunnel_url when exposeTunnelToken is on.
	listenAddr string
}

// ServerOption customizes a Server at construction. Options keep the NewServer
// signature stable (existing NewServer(st, dev) call sites compile unchanged)
// while letting main.go opt into the dev token-exposure seam.
type ServerOption func(*Server)

// WithExposeTunnelToken enables (DEV-ONLY) inclusion of tunnel_token/tunnel_url
// in the POST /sessions response. Driven by ECU_DEV_EXPOSE_TUNNEL_TOKEN=1.
func WithExposeTunnelToken(expose bool) ServerOption {
	return func(s *Server) { s.exposeTunnelToken = expose }
}

// WithListenAddr sets the listen address used to build the ws:// tunnel_url in
// the dev token-exposure response (e.g. "127.0.0.1:8080").
func WithListenAddr(addr string) ServerOption {
	return func(s *Server) { s.listenAddr = addr }
}

// NewServer builds a Server. The store is used for auth and session records;
// devToolServer is the ECU_DEV_TOOLSERVER seam (may be empty). The tool proxy's
// SessionTransport is a composite that PREFERS a live reverse tunnel (C3) and
// FALLS BACK to a directTransport for the dev seam. Both sides resolve the
// upstream behind the SessionTransport interface, so no handler ever touches a
// tool-server address.
func NewServer(st *store.Store, devToolServer string, opts ...ServerOption) *Server {
	s := &Server{
		store:         st,
		registry:      newTunnelRegistry(),
		devToolServer: devToolServer,
	}
	for _, opt := range opts {
		opt(s)
	}
	// Composite: tunnel preferred, direct dev endpoint as fallback. When
	// devToolServer is empty the direct side simply never resolves (ok=false),
	// so only the tunnel path works; when it is set, sessions without a live
	// tunnel still reach the dev tool server. The two flows coexist.
	tun := newTunnelTransport(s.registry)
	direct := newDirectTransport(s.resolveEndpoint)
	s.transport = newCompositeTransport(tun, direct)
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
// method+wildcard patterns (Go 1.22+).
//
// All /sessions* routes stay behind the API-key auth middleware. The C3 tunnel
// ingress GET /agent/connect is the ONE exception — it authenticates with a
// per-session tunnel token, not an API key, so the (path-aware) authMiddleware
// skips it and the handler performs its own token check before upgrading. We
// keep a single mux (rather than splitting protected/root muxes) so the
// existing method-based pattern matching for /sessions and /sessions/{id}... is
// untouched; the exemption is a single path check in authMiddleware, which is
// clearly correct and keeps every existing auth test green.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{id}/{action}", s.handleAction)
	mux.HandleFunc("GET /agent/connect", s.handleAgentConnect)
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
