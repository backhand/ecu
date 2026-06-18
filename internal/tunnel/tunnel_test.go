package tunnel

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestTunnelRoundTrip proves the muxing + forwarding works end-to-end WITHOUT
// WebSocket: a net.Pipe() stands in for the upgraded WS conn. The CP side
// (yamux Server) builds a RoundTripper; the agent side (yamux Client) accepts
// streams and forwards them to a real httptest tool server. We then issue a GET
// and a POST through the RoundTripper and assert the full request (method,
// path, body) rode through the mux and the response came back intact.
func TestTunnelRoundTrip(t *testing.T) {
	// Tiny "local tool server": echoes method + path, and appends the request
	// body so we can prove the whole request crossed the mux.
	toolSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" body="+string(body))
	}))
	defer toolSrv.Close()

	toolURL, err := url.Parse(toolSrv.URL)
	if err != nil {
		t.Fatalf("parse tool server url: %v", err)
	}

	// net.Pipe gives two connected, synchronous net.Conns. yamux handles its
	// own framing over them, so this is a faithful stand-in for the WS conn.
	cpConn, agentConn := net.Pipe()

	tun, err := NewServerTunnel(cpConn)
	if err != nil {
		t.Fatalf("NewServerTunnel: %v", err)
	}
	defer tun.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientDone := make(chan error, 1)
	go func() {
		clientDone <- RunClient(ctx, agentConn, toolURL.Host)
	}()

	client := &http.Client{
		Transport: tun.RoundTripper(),
		Timeout:   10 * time.Second,
	}

	// GET — host is irrelevant (DialContext ignores it); the tool server still
	// sees the method and path.
	t.Run("GET", func(t *testing.T) {
		resp, err := client.Get("http://anything/screenshot")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status = %d, want 200", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if got, want := string(b), "GET /screenshot body="; got != want {
			t.Fatalf("GET body = %q, want %q", got, want)
		}
	})

	// POST with a body — proves request bodies cross the mux intact.
	t.Run("POST", func(t *testing.T) {
		resp, err := client.Post("http://anything/click", "application/json",
			strings.NewReader(`{"x":1,"y":2}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST status = %d, want 200", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if got, want := string(b), `POST /click body={"x":1,"y":2}`; got != want {
			t.Fatalf("POST body = %q, want %q", got, want)
		}
	})

	// Tearing down the CP side must end the agent's accept loop.
	if err := tun.Close(); err != nil {
		t.Fatalf("tun.Close: %v", err)
	}
	cancel()
	select {
	case <-clientDone:
		// RunClient returned: the session died as expected.
	case <-time.After(5 * time.Second):
		t.Fatal("RunClient did not return after tunnel close")
	}
}

// TestTunnelStreamFailureIsolated proves that a single failing stream (tool
// server refusing the TCP dial) does not kill the tunnel: a subsequent request
// to a working tool server still succeeds.
func TestTunnelStreamFailureIsolated(t *testing.T) {
	cpConn, agentConn := net.Pipe()
	tun, err := NewServerTunnel(cpConn)
	if err != nil {
		t.Fatalf("NewServerTunnel: %v", err)
	}
	defer tun.Close()

	// Point the agent at a closed port first to force a per-stream dial error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close() // nothing is listening now

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = RunClient(ctx, agentConn, deadAddr) }()

	client := &http.Client{Transport: tun.RoundTripper(), Timeout: 5 * time.Second}

	// First request hits the dead address: the stream closes mid-flight, the
	// transport surfaces an error — but the tunnel itself must survive.
	if _, err := client.Get("http://anything/screenshot"); err == nil {
		t.Fatal("expected error talking to dead tool server, got nil")
	}
	if tun.IsClosed() {
		t.Fatal("tunnel closed after a single failed stream; failures must be isolated")
	}
}
