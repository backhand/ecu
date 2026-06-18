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
	"sync"
	"time"

	"github.com/backhand/ecu/internal/provider"
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

	// provider is the cloud Provider used on the production path to create and
	// destroy instances (C4). It is nil in dev-toolserver mode (which never
	// provisions) and in C2/C3 tests that don't exercise provisioning.
	provider provider.Provider

	// providerName is the configured provider name (ECU_PROVIDER), used to gate
	// provider-capability-specific behavior — notably rejecting persistent /
	// restore requests on the local provider, which cannot snapshot. Empty in
	// tests that don't set it; the gate only fires when it equals "local".
	providerName string

	// provisionCfg carries everything the production POST /sessions path needs
	// to render cloud-init and create an instance (see ProvisionConfig).
	provisionCfg ProvisionConfig

	// reaperCfg carries the C5 reaper timeouts/interval (see ReaperConfig). The
	// zero value disables the idle/lifetime rules and uses a default interval;
	// the orphan rule still applies (it is the leak backstop).
	reaperCfg ReaperConfig

	// maxSessions is the global cap on ACTIVE (provisioning+ready) sessions
	// enforced by POST /sessions (C5). 0 means unlimited. Set via
	// WithMaxSessions.
	maxSessions int

	// maxPersistentSessions is the cap on NON-TERMINATED persistent sessions
	// (provisioning + ready + stopped) enforced by the persistent POST /sessions
	// path (C8); a stopped session counts until it is culled. 0 means unlimited
	// (no persistent cap). Set via WithMaxPersistentSessions.
	maxPersistentSessions int

	// now is the clock the reaper reads. It is time.Now in production and is
	// overridden in tests (WithClock) so idle/lifetime/orphan windows can be
	// driven deterministically without real sleeping. ONLY the reaper uses it;
	// the broker and store stamp real wall-clock time.
	now func() time.Time

	// bakeCfg carries the C7 pre-bake settings (see BakeConfig). Only consulted
	// by StartBake, which main invokes on startup when ECU_IMAGE is set.
	bakeCfg BakeConfig

	// bakeRegistry maps a per-bake token to the channel its outbound completion
	// callback fires. The (API-key-exempt, token-authed) bake callback handler
	// reads it; the baker registers/unregisters around each bake.
	bakeRegistry *bakeRegistry

	// activeBootImage is the image reference NEW sessions boot from once a bake
	// completes (or a pre-existing snapshot is found at startup). Empty means
	// "not set yet" and ActiveBootImage falls back to provisionCfg.BaseImage (the
	// cold-boot OS image). Guarded by bootImageMu so the baker (writer) and the
	// provisioning flow (reader) never race.
	activeBootImage string
	bootImageMu     sync.RWMutex

	// signingKey is the HMAC secret for live-watch view tokens (C9). It is set in
	// NewServer from ECU_SIGNING_KEY or a random 32 bytes (WithSigningKey). Never
	// empty after construction.
	signingKey []byte

	// publicBaseURL is the externally reachable base (e.g. "https://ecu.example.com"
	// or "http://127.0.0.1:8080" in dev) used to build the absolute watch_url in
	// GET /sessions status (C9). Empty leaves watch_url null (no public base
	// configured). Set via WithPublicBaseURL.
	publicBaseURL string
}

// ProvisionConfig carries the settings the production provisioning path needs.
// It is supplied via WithProvisionConfig from cmd/ecu (derived from the loaded
// config) and is unused in dev-toolserver mode.
type ProvisionConfig struct {
	// TunnelURL is the publicly reachable tunnel ingress the agent dials OUT
	// to, e.g. "wss://ecu.example.com/agent/connect". For real cloud instances
	// this MUST be reachable from the instance (hostname + TLS; C10). In local
	// dev it may be "ws://<listen>/agent/connect".
	TunnelURL string

	// ImageRef is the container image cloud-init runs on the instance.
	ImageRef string

	// AgentBinaryURL is where the instance fetches the ecu binary from.
	AgentBinaryURL string

	// InstanceType / Region / BaseImage configure the created instance.
	InstanceType string
	Region       string
	BaseImage    string

	// Width / Height are the desktop resolution passed to the container.
	Width, Height int

	// ProvisionTimeout bounds how long the background waiter waits for the
	// agent to register before tearing the instance down. Must be > 0 on the
	// production path; tests inject a short value.
	ProvisionTimeout time.Duration
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

// WithProvider sets the cloud Provider used by the production POST /sessions
// path to create instances and by DELETE to destroy them (C4). A nil provider
// is fine in dev-toolserver mode (that path never calls it).
func WithProvider(p provider.Provider) ServerOption {
	return func(s *Server) { s.provider = p }
}

// WithProvisionConfig supplies the settings the production provisioning path
// needs (cloud-init inputs, instance shape, and the readiness timeout).
func WithProvisionConfig(cfg ProvisionConfig) ServerOption {
	return func(s *Server) { s.provisionCfg = cfg }
}

// WithProviderName records the configured provider name (ECU_PROVIDER) so the
// server can gate provider-capability-specific behavior, notably rejecting
// persistent / restore requests on the local provider (which cannot snapshot).
func WithProviderName(name string) ServerOption {
	return func(s *Server) { s.providerName = name }
}

// WithBakeConfig supplies the C7 pre-bake settings (see BakeConfig). It is set
// when ECU_IMAGE is configured; main then calls StartBake on startup. Without
// it (ECU_IMAGE unset) no bake runs and sessions cold-boot, unchanged.
func WithBakeConfig(cfg BakeConfig) ServerOption {
	return func(s *Server) { s.bakeCfg = cfg }
}

// WithReaperConfig supplies the C5 reaper's timeouts and tick interval (see
// ReaperConfig). Without it the reaper runs with idle/lifetime disabled and a
// default interval; the orphan rule still protects against leaked instances.
func WithReaperConfig(cfg ReaperConfig) ServerOption {
	return func(s *Server) { s.reaperCfg = cfg }
}

// WithMaxSessions sets the global cap on concurrently ACTIVE
// (provisioning+ready) sessions enforced by POST /sessions. A value of 0 (the
// default) means UNLIMITED — no cap is applied. error/terminated sessions never
// count toward the cap.
func WithMaxSessions(n int) ServerOption {
	return func(s *Server) { s.maxSessions = n }
}

// WithMaxPersistentSessions sets the cap on NON-TERMINATED persistent sessions
// (provisioning + ready + stopped) enforced by the persistent POST /sessions
// path (C8). A value of 0 means UNLIMITED. A stopped session still counts (it
// holds saved state + a snapshot) until it is culled to terminated, which is
// the only transition that frees a slot.
func WithMaxPersistentSessions(n int) ServerOption {
	return func(s *Server) { s.maxPersistentSessions = n }
}

// WithSigningKey sets the HMAC secret for live-watch view tokens (C9). An empty
// key means "generate a random one in NewServer" (tokens then don't survive a
// restart — fine for minutes-long view tokens). main passes ECU_SIGNING_KEY.
func WithSigningKey(key string) ServerOption {
	return func(s *Server) {
		if key != "" {
			s.signingKey = []byte(key)
		}
	}
}

// WithPublicBaseURL sets the externally reachable base URL used to build the
// absolute watch_url in GET /sessions status (C9), e.g.
// "https://ecu.example.com" or "http://127.0.0.1:8080" in dev. Empty (the
// default) leaves watch_url null.
func WithPublicBaseURL(base string) ServerOption {
	return func(s *Server) { s.publicBaseURL = base }
}

// WithClock overrides the clock the reaper reads (default time.Now). It exists
// for deterministic tests, which advance a fake clock instead of sleeping. Only
// the reaper consults this clock; the broker stamps real wall-clock time so the
// idle/lifetime backstops cannot be skewed by a test clock in production code
// paths.
func WithClock(now func() time.Time) ServerOption {
	return func(s *Server) { s.now = now }
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
		bakeRegistry:  newBakeRegistry(),
		devToolServer: devToolServer,
	}
	for _, opt := range opts {
		opt(s)
	}
	// Default the reaper clock to wall-clock time unless a test injected one.
	if s.now == nil {
		s.now = time.Now
	}
	// Default the watch-token signing key to a random 32 bytes when none was
	// configured (WithSigningKey left it empty). A random key invalidates view
	// tokens across restarts — acceptable for a minutes-long token.
	if len(s.signingKey) == 0 {
		s.signingKey, _ = resolveSigningKey("")
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
// All /sessions* routes stay behind the API-key auth middleware. TWO routes are
// exceptions, each authenticated by its own per-operation token rather than an
// API key (so the path-aware authMiddleware skips them and each handler does its
// own constant-time token check):
//   - GET /agent/connect — the C3 tunnel ingress (per-session tunnel token).
//   - POST /internal/bake/{token}/done — the C7 bake-completion callback fired
//     OUTBOUND by a bake instance (per-bake token). A bake instance has no API
//     key, so it must be exempt, exactly like /agent/connect.
//
// We keep a single mux (rather than splitting protected/root muxes) so the
// existing method-based pattern matching for /sessions and /sessions/{id}... is
// untouched; the exemptions are path checks in authMiddleware, which are clearly
// correct and keep every existing auth test green.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("GET /sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /sessions/{id}/{action}", s.handleAction)
	// C9 live watch: a human-watchable noVNC view proxied through the session's
	// tunnel, API-key-EXEMPT and gated by a short-lived view token/cookie inside
	// handleWatch. The bare path and the asset/websockify sub-paths are separate
	// patterns (Go's mux needs the {rest...} catch-all to be its own route); the
	// bare one calls handleWatch with an empty {rest}.
	mux.HandleFunc("GET /sessions/{id}/watch", s.handleWatch)
	mux.HandleFunc("GET /sessions/{id}/watch/{rest...}", s.handleWatch)
	mux.HandleFunc("GET /agent/connect", s.handleAgentConnect)
	mux.HandleFunc("POST /internal/bake/{token}/done", s.handleBakeDone)
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
