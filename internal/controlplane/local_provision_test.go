package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/provider/fake"
)

// boolp returns a pointer to b, for setting the fake provider's
// RequiresCloudInitResult knob.
func boolp(b bool) *bool { return &b }

// TestDirectProvisionMarksReadyWithEndpoint exercises the local-provider
// direct-provision path end to end via the fake provider: the provider returns
// an Instance.Endpoint (no tunnel) and reports RequiresCloudInit()==false. The
// provisioning flow must:
//   - render NO cloud-init (the headline assertion: LastUserData stays ""),
//   - persist the endpoint as the session's tool endpoint,
//   - flip the session to ready WITHOUT waiting for a tunnel,
//   - and serve a proxied action through the direct transport to that endpoint.
func TestDirectProvisionMarksReadyWithEndpoint(t *testing.T) {
	st := newTestStore(t)

	// A stand-in tool server: 200 to anything (so /healthz-style checks and the
	// proxied /screenshot both succeed), with a marker body proving a proxied
	// action actually landed here.
	const marker = `{"hit":"toolserver"}`
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, marker)
	}))
	defer toolServer.Close()

	prov := fake.New()
	prov.CreateEndpoint = toolServer.URL
	prov.RequiresCloudInitResult = boolp(false)

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: 5 * time.Second})

	cr := createSession(t, ts)
	if cr.Status != statusProvisioning {
		t.Fatalf("created status = %q, want provisioning", cr.Status)
	}

	// The direct path flips to ready off the provider's health wait — no agent.
	waitForStatus(t, st, cr.SessionID, statusReady, 5*time.Second)

	// The endpoint was persisted as the session's tool endpoint.
	sess, found, err := st.GetSession(cr.SessionID)
	if err != nil || !found {
		t.Fatalf("GetSession(%s): found=%v err=%v", cr.SessionID, found, err)
	}
	if sess.ToolEndpoint != prov.CreateEndpoint {
		t.Fatalf("ToolEndpoint = %q, want %q", sess.ToolEndpoint, prov.CreateEndpoint)
	}

	// Headline: the local path rendered NO cloud-init.
	if ud := prov.LastUserData(); ud != "" {
		t.Fatalf("LastUserData = %q, want empty (local provider must not render cloud-init)", ud)
	}

	// A proxied action rides the composite transport's direct fallback (no tunnel
	// exists for a local session) straight to the tool server.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions/"+cr.SessionID+"/screenshot", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer k_active")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST screenshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxied screenshot status = %d body=%s, want 200", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hit":"toolserver"`) {
		t.Fatalf("proxied response body = %q, want it to carry the tool-server marker", body)
	}
}

// TestLocalProviderRejectsPersistent verifies POST /sessions {"persistent":true}
// is rejected with 400 and the local-provider detail — BEFORE the 403
// persistent-capability check (k_active HAS that capability, so a 400 here proves
// the local gate fires first).
func TestLocalProviderRejectsPersistent(t *testing.T) {
	ts, _ := newLocalProviderServer(t)

	status, detail := postSession(t, ts, `{"persistent":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("persistent POST status = %d, want 400", status)
	}
	if detail != "persistence is not supported with the local provider" {
		t.Fatalf("detail = %q, want the local-provider rejection", detail)
	}
}

// TestLocalProviderRejectsRestore verifies POST /sessions {"restore":"x"} is
// rejected with the same 400 + detail.
func TestLocalProviderRejectsRestore(t *testing.T) {
	ts, _ := newLocalProviderServer(t)

	status, detail := postSession(t, ts, `{"restore":"x"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("restore POST status = %d, want 400", status)
	}
	if detail != "persistence is not supported with the local provider" {
		t.Fatalf("detail = %q, want the local-provider rejection", detail)
	}
}

// TestLocalProviderAllowsEphemeral verifies an ephemeral POST /sessions (empty
// body) is NOT gated: it returns 200 with status provisioning. This guards that
// the local gate is scoped to persistent/restore only.
func TestLocalProviderAllowsEphemeral(t *testing.T) {
	ts, _ := newLocalProviderServer(t)

	status, _ := postSession(t, ts, `{}`)
	if status != http.StatusOK {
		t.Fatalf("ephemeral POST status = %d, want 200", status)
	}
}

// newLocalProviderServer constructs a control plane with WithProviderName("local")
// and a fake provider that returns a tool-server endpoint (so an ephemeral create
// can complete) and reports RequiresCloudInit()==false. Returns the server and
// the fake provider.
func newLocalProviderServer(t *testing.T) (*httptest.Server, *fake.Provider) {
	t.Helper()
	st := newTestStore(t)

	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(toolServer.Close)

	prov := fake.New()
	prov.RequiresCloudInitResult = boolp(false)
	prov.CreateEndpoint = toolServer.URL

	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	srv := NewServer(st, "",
		WithListenAddr(addr),
		WithProvider(prov),
		WithProvisionConfig(ProvisionConfig{
			TunnelURL:        "ws://" + addr + agentConnectPath,
			AgentBinaryURL:   "https://example.invalid/ecu",
			ProvisionTimeout: 5 * time.Second,
		}),
		WithProviderName("local"),
	)
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, prov
}

// postSession POSTs body to /sessions as k_active and returns the status code
// and the JSON "detail" field (empty if absent).
func postSession(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k_active")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /sessions: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m["detail"]
}
