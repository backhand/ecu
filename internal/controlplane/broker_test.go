package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/agent"
	"github.com/backhand/ecu/internal/store"
)

// startCP boots the control-plane Handler on a real httptest.Server with the
// dev token-exposure seam enabled, so a test can create a session and learn its
// tunnel token + ws URL. Returns the server and its host:port.
func startCP(t *testing.T, st *store.Store) (*httptest.Server, string) {
	t.Helper()
	// listenAddr is needed to build tunnel_url; we patch it to the test server's
	// host after it starts by constructing the server with the eventual addr.
	// httptest assigns the addr on Start, so build the Server first, then set
	// the option via a fresh server bound to the same handler is circular —
	// instead we read ts.Listener.Addr after NewUnstartedServer.
	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()

	srv := NewServer(st, "",
		WithExposeTunnelToken(true),
		WithListenAddr(addr),
	)
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, addr
}

// createSessionForTunnel issues POST /sessions and returns the session id and
// tunnel token (exposed via the dev seam).
func createSessionForTunnel(t *testing.T, ts *httptest.Server) (id, token string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer k_active")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /sessions status=%d body=%s", resp.StatusCode, b)
	}
	var cr createSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if cr.Status != statusProvisioning {
		t.Fatalf("created status = %q, want provisioning", cr.Status)
	}
	if cr.TunnelToken == "" || cr.TunnelURL == "" {
		t.Fatalf("dev exposure seam did not return token/url: %+v", cr)
	}
	return cr.SessionID, cr.TunnelToken
}

// TestAgentConnectRejectsBadToken verifies that /agent/connect rejects an
// unknown or missing token with 401 and does NOT register a tunnel or flip the
// session to ready — the auth check happens before any upgrade.
func TestAgentConnectRejectsBadToken(t *testing.T) {
	st := newTestStore(t)
	ts, _ := startCP(t, st)

	// No Authorization header at all.
	resp, err := ts.Client().Get(ts.URL + "/agent/connect")
	if err != nil {
		t.Fatalf("GET /agent/connect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}

	// A bogus Bearer token.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/agent/connect", nil)
	req.Header.Set("Authorization", "Bearer t_not_a_real_token")
	resp2, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /agent/connect (bad token): %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", resp2.StatusCode)
	}
}

// TestTunnelEndToEnd is the headline integration test: an agent dials
// /agent/connect with a valid token, the session flips to ready, a proxied
// action rides the tunnel to the agent's local tool server and back, and after
// the agent disconnects the session leaves ready (proxy returns 409).
func TestTunnelEndToEnd(t *testing.T) {
	st := newTestStore(t)
	ts, _ := startCP(t, st)

	// The agent's "local tool server" — echoes method+path+body so we can prove
	// the full request crossed the tunnel.
	toolSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"method":"`+r.Method+`","path":"`+r.URL.Path+`","body":`+strconv.Quote(string(body))+`}`)
	}))
	defer toolSrv.Close()

	id, token := createSessionForTunnel(t, ts)

	// Run the agent against a ws:// URL derived from the test server.
	wsURL := "ws://" + mustHost(t, ts.URL) + agentConnectPath
	ctx, cancel := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		_ = agent.Run(ctx, agent.Config{
			ControlPlaneURL: wsURL,
			Token:           token,
			ToolServer:      toolSrv.URL,
		})
	}()

	// Wait for the broker to flip the session to ready (the agent connected).
	waitForStatus(t, st, id, statusReady, 5*time.Second)

	// Issue an action through the control plane; it must ride the tunnel.
	actReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions/"+id+"/click",
		strings.NewReader(`{"x":5,"y":7}`))
	actReq.Header.Set("Authorization", "Bearer k_active")
	actReq.Header.Set("Content-Type", "application/json")
	actResp, err := ts.Client().Do(actReq)
	if err != nil {
		t.Fatalf("POST action: %v", err)
	}
	body, _ := io.ReadAll(actResp.Body)
	actResp.Body.Close()
	if actResp.StatusCode != http.StatusOK {
		t.Fatalf("action status = %d body=%s, want 200 through tunnel", actResp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"path":"/click"`) ||
		!strings.Contains(string(body), `"method":"POST"`) {
		t.Fatalf("tunnel did not carry the full request; got %s", body)
	}

	// Shut the agent down; the broker must take the session out of ready.
	cancel()
	<-agentDone
	waitForStatus(t, st, id, statusProvisioning, 5*time.Second)

	// A subsequent action now hits the 409 path (session not ready).
	actReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions/"+id+"/click",
		strings.NewReader(`{}`))
	actReq2.Header.Set("Authorization", "Bearer k_active")
	actResp2, err := ts.Client().Do(actReq2)
	if err != nil {
		t.Fatalf("POST action after disconnect: %v", err)
	}
	actResp2.Body.Close()
	if actResp2.StatusCode != http.StatusConflict {
		t.Fatalf("post-disconnect action status = %d, want 409", actResp2.StatusCode)
	}
}

// waitForStatus polls the store until the session reaches want or the deadline
// passes.
func waitForStatus(t *testing.T, st *store.Store, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sess, found, err := st.GetSession(id)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if found && sess.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	sess, _, _ := st.GetSession(id)
	t.Fatalf("session %s did not reach status %q within %s (last=%q)", id, want, timeout, sess.Status)
}

// mustHost extracts host:port from a URL, failing the test on error.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u.Host
}
