// Package tunnel implements the reverse transport that lets the control plane
// (CP) reach a session's tool server over a connection the instance dials OUT —
// so the instance needs no inbound ports. The byte transport is a single
// WebSocket connection (github.com/coder/websocket) carrying a yamux
// (github.com/hashicorp/yamux) multiplexer.
//
// Role assignment (this is load-bearing — mismatched roles DEADLOCK):
//
//   - The CP is the yamux SERVER (NewServerTunnel → yamux.Server). It OPENS a
//     stream per outbound HTTP request via *http.Transport.DialContext.
//   - The agent is the yamux CLIENT (RunClient → yamux.Client). It ACCEPTs each
//     stream and proxies the raw bytes to the local tool server over TCP.
//
// yamux requires exactly one Server and one Client over the single conn; both
// can Open() and Accept(), but the pairing must be exactly one of each. The CP
// only Opens; the agent only Accepts.
//
// The pipe is protocol-agnostic: the CP writes a complete HTTP/1.1 request onto
// the stream and the tool server answers HTTP/1.1 on the same stream. tunnel
// never parses the bytes, so screenshots and any future endpoints ride through
// unchanged. (The WS read limit must be raised on both ends so large frames —
// e.g. screenshots — are not truncated; that is the caller's responsibility, in
// internal/controlplane/broker.go and internal/agent.)
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"
)

// keepAliveInterval is how often each yamux session pings its peer. It must be
// strictly less than ConnectionWriteTimeout (yamux.VerifyConfig enforces this),
// so we keep the write timeout at its 10s default and ping every 5s. Keepalive
// lets a half-dead tunnel (instance vanished, NAT dropped the flow) be detected
// promptly so the broker can flip the session out of "ready".
const keepAliveInterval = 5 * time.Second

// yamuxConfig builds the shared yamux configuration used by both roles. It must
// be byte-for-byte compatible across the two ends (it is — both call this), and
// it must pass yamux.VerifyConfig (verified by yamux.Server/Client themselves).
// LogOutput is pinned to io.Discard so a dying tunnel does not spam stderr; the
// CP/agent log lifecycle events themselves.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = keepAliveInterval
	// ConnectionWriteTimeout stays at the 10s default (> keepAliveInterval).
	cfg.LogOutput = io.Discard
	return cfg
}

// Tunnel wraps the CP-side (yamux Server) session. Each outbound HTTP request
// rides a fresh yamux stream obtained via the RoundTripper's DialContext, so
// requests are concurrent and pooled over the one WebSocket conn.
type Tunnel struct {
	session *yamux.Session
	rt      http.RoundTripper
}

// NewServerTunnel builds the CP side of the tunnel over an already-upgraded
// WebSocket net.Conn. conn must be obtained with a context that spans the WHOLE
// tunnel lifetime (NOT a per-request context); when that context is cancelled
// the underlying WS conn fails and the yamux session closes.
//
// It returns a *Tunnel whose RoundTripper opens one yamux stream per HTTP
// request. The caller owns conn's lifetime via Close.
func NewServerTunnel(conn net.Conn) (*Tunnel, error) {
	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		return nil, fmt.Errorf("tunnel: building yamux server session: %w", err)
	}
	t := &Tunnel{session: session}
	t.rt = newStreamTransport(session)
	return t, nil
}

// RoundTripper returns an http.RoundTripper that dials each request onto a new
// yamux stream. It is safe to reuse across requests and across goroutines; the
// underlying *http.Transport pools idle streams.
func (t *Tunnel) RoundTripper() http.RoundTripper { return t.rt }

// Wait returns a channel that is closed when the yamux session dies (peer gone,
// keepalive failed, or Close called). The broker blocks on this to know when to
// deregister the session and flip it out of "ready".
func (t *Tunnel) Wait() <-chan struct{} { return t.session.CloseChan() }

// IsClosed reports whether the session has shut down.
func (t *Tunnel) IsClosed() bool { return t.session.IsClosed() }

// Close tears down the yamux session (and thus all live streams). It is safe to
// call more than once.
func (t *Tunnel) Close() error { return t.session.Close() }

// newStreamTransport builds an *http.Transport whose DialContext ignores the
// network/address entirely and instead opens a fresh yamux stream on session.
// Because every "dial" is a stream Open on the one multiplexed conn:
//
//   - HTTP/1.1 only (ForceAttemptHTTP2=false; TLSClientConfig stays nil — the
//     stream carries plaintext h1, the WS layer provides any transport security).
//   - Keep-alives stay ENABLED so the transport pools idle streams instead of
//     opening a new one per request.
func newStreamTransport(session *yamux.Session) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Ignore network/addr: the only destination is the peer across the
			// mux. Open returns a *yamux.Stream (a net.Conn).
			return session.Open()
		},
		// Pool streams across requests.
		DisableKeepAlives:   false,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     0, // unbounded; yamux backpressures via its window
		IdleConnTimeout:     90 * time.Second,
		// Plaintext HTTP/1.1 over the stream.
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       nil,
		ExpectContinueTimeout: time.Second,
	}
}
