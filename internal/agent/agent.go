// Package agent implements the instance-side end of the ECU reverse tunnel. It
// dials OUT from the instance to the control plane's /agent/connect WebSocket,
// authenticates with the session's tunnel token, and then runs the tunnel
// CLIENT (see internal/tunnel): every yamux stream the control plane opens is
// spliced to the local tool server. Because the connection is outbound, the
// instance needs no inbound ports.
//
// Run reconnects with capped exponential backoff so a transient control-plane
// restart or network blip is recovered automatically; it returns only when its
// context is cancelled (SIGINT/SIGTERM in cmd/ecu).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/backhand/ecu/internal/tunnel"
	"github.com/coder/websocket"
)

// Config holds the agent's runtime settings, supplied by cmd/ecu from flags and
// ECU_AGENT_* env fallbacks.
type Config struct {
	// ControlPlaneURL is the ws:// (or wss://) URL of the control-plane tunnel
	// ingress, e.g. "ws://127.0.0.1:8080/agent/connect".
	ControlPlaneURL string

	// Token is the session-scoped tunnel token presented as a Bearer header.
	Token string

	// ToolServer is the base URL of the local tool server, e.g.
	// "http://127.0.0.1:8000". Only its host:port is used (the tunnel carries
	// raw HTTP/1.1 bytes to a TCP dial).
	ToolServer string
}

// backoff bounds for the reconnect loop.
const (
	backoffInitial = 1 * time.Second
	backoffMax     = 30 * time.Second
	// stableConnection is how long a tunnel must stay up before we treat the
	// connection as "good" and reset the backoff to its initial value.
	stableConnection = 30 * time.Second
)

// Run connects the agent to the control plane and keeps the reverse tunnel
// alive, reconnecting with capped exponential backoff until ctx is cancelled.
// It validates required config up front and returns an error for invalid input;
// transient connection failures are retried rather than returned.
func Run(ctx context.Context, cfg Config) error {
	if cfg.ControlPlaneURL == "" {
		return errors.New("agent: control-plane URL is required")
	}
	if cfg.Token == "" {
		return errors.New("agent: tunnel token is required")
	}
	if cfg.ToolServer == "" {
		cfg.ToolServer = "http://127.0.0.1:8000"
	}
	toolAddr, err := toolServerHostPort(cfg.ToolServer)
	if err != nil {
		return fmt.Errorf("agent: invalid tool-server URL %q: %w", cfg.ToolServer, err)
	}

	log.Printf("ecu agent: starting; control-plane=%s tool-server=%s", cfg.ControlPlaneURL, cfg.ToolServer)

	backoff := backoffInitial
	for {
		if ctx.Err() != nil {
			return nil
		}

		start := time.Now()
		err := connectOnce(ctx, cfg, toolAddr)
		if ctx.Err() != nil {
			// Cancellation is a clean shutdown, not a failure.
			return nil
		}

		// If the tunnel was up long enough, treat it as a healthy connection and
		// reset the backoff; otherwise grow it (capped).
		if time.Since(start) >= stableConnection {
			backoff = backoffInitial
		}

		if err != nil {
			log.Printf("ecu agent: disconnected: %v; reconnecting in %s", err, backoff)
		} else {
			log.Printf("ecu agent: tunnel closed; reconnecting in %s", backoff)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < backoffMax {
			backoff *= 2
			if backoff > backoffMax {
				backoff = backoffMax
			}
		}
	}
}

// connectOnce performs a single dial + tunnel lifetime. It returns when the
// tunnel dies (or ctx is cancelled). A returned error describes the failure for
// logging; the caller decides whether to retry.
func connectOnce(ctx context.Context, cfg Config, toolAddr string) error {
	// connCtx spans the WHOLE tunnel lifetime (never a per-request context).
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	log.Printf("ecu agent: connecting to %s", cfg.ControlPlaneURL)
	c, resp, err := websocket.Dial(connCtx, cfg.ControlPlaneURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.Token}},
	})
	if err != nil {
		// On a handshake failure coder/websocket may return the *http.Response.
		// A 401 means a bad/expired token; surface it clearly but still let the
		// caller back off and retry (the token may be rotated, or the session
		// may not yet be persisted on the CP).
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("authentication rejected (401): check the tunnel token")
		}
		if resp != nil {
			return fmt.Errorf("dial failed (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("dial failed: %w", err)
	}
	// Raise the read limit so large frames (e.g. screenshots) are not truncated;
	// MUST match the control-plane side. Set before wrapping with NetConn.
	c.SetReadLimit(-1)
	// Ensure the underlying conn is torn down when we return.
	defer c.CloseNow()

	log.Printf("ecu agent: connected; forwarding streams to %s", toolAddr)
	netConn := websocket.NetConn(connCtx, c, websocket.MessageBinary)

	// RunClient blocks until the yamux session dies (or connCtx is cancelled).
	return tunnel.RunClient(connCtx, netConn, toolAddr)
}

// toolServerHostPort parses a tool-server base URL and returns its host:port
// suitable for net.Dial("tcp", ...). When the URL omits a port it defaults to
// 80 for http and 443 for https.
func toolServerHostPort(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("missing host in %q", base)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	switch u.Scheme {
	case "https":
		return u.Hostname() + ":443", nil
	default: // http or unspecified
		return u.Hostname() + ":80", nil
	}
}
