package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/agent"
	"github.com/backhand/ecu/internal/provider"
	"github.com/backhand/ecu/internal/provider/fake"
	"github.com/backhand/ecu/internal/store"
)

// startCPWithProvider boots the control-plane Handler on a real httptest.Server
// wired with the given fake provider and provision config. The dev
// token-exposure seam is left OFF (production path): the provisioning flow is
// driven entirely through the provider + agent, and the token is read out of
// the cloud-init UserData the fake captures. Returns the server and its
// host:port.
func startCPWithProvider(t *testing.T, st *store.Store, prov provider.Provider, pc ProvisionConfig) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	// Default the tunnel URL to the test server's ws:// address unless the
	// caller set one (the agent dials this).
	if pc.TunnelURL == "" {
		pc.TunnelURL = "ws://" + addr + agentConnectPath
	}
	if pc.AgentBinaryURL == "" {
		pc.AgentBinaryURL = "https://example.invalid/ecu" // unused; cloud-init isn't executed in tests
	}
	srv := NewServer(st, "",
		WithListenAddr(addr),
		WithProvider(prov),
		WithProvisionConfig(pc),
	)
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, addr
}

// createSession issues POST /sessions and returns the decoded response.
func createSession(t *testing.T, ts *httptest.Server) createSessionResponse {
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
	return cr
}

var tokenInUserData = regexp.MustCompile(`--token '([^']+)'`)

// tokenFromUserData extracts the tunnel token from rendered cloud-init.
func tokenFromUserData(t *testing.T, userData string) string {
	t.Helper()
	m := tokenInUserData.FindStringSubmatch(userData)
	if len(m) != 2 {
		t.Fatalf("could not find --token in UserData:\n%s", userData)
	}
	return m[1]
}

// TestProvisionHappyPath is the headline flow test: POST /sessions returns
// `provisioning` immediately; the provider's CreateInstance is called once with
// cloud-init carrying the session's tunnel token + the ws URL; the fake drives
// a real agent to connect (flipping the session to ready); and the instance is
// NOT torn down.
func TestProvisionHappyPath(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()

	// The agent's local tool server (echoes so a proxied action could ride the
	// tunnel; here we just need it reachable).
	toolSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer toolSrv.Close()

	ts, addr := startCPWithProvider(t, st, prov, ProvisionConfig{
		ProvisionTimeout: 5 * time.Second,
	})
	wsURL := "ws://" + addr + agentConnectPath

	// On create, launch the real agent in a goroutine using the token parsed
	// out of the cloud-init the control plane rendered. This is what drives the
	// session to ready (the broker flips it when the tunnel registers).
	agentCtx, cancelAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	prov.OnCreate = func(spec provider.InstanceSpec) {
		token := tokenFromUserData(t, spec.UserData)
		go func() {
			defer close(agentDone)
			_ = agent.Run(agentCtx, agent.Config{
				ControlPlaneURL: wsURL,
				Token:           token,
				ToolServer:      toolSrv.URL,
			})
		}()
	}
	t.Cleanup(func() { cancelAgent(); <-agentDone })

	cr := createSession(t, ts)
	if cr.Status != statusProvisioning {
		t.Fatalf("POST /sessions returned status %q, want provisioning (immediate)", cr.Status)
	}

	// The session reaches ready once the agent connects.
	waitForStatus(t, st, cr.SessionID, statusReady, 5*time.Second)

	if prov.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", prov.CreateCount())
	}

	// The captured cloud-init carries the session's tunnel token and the ws URL.
	sess, _, _ := st.GetSession(cr.SessionID)
	ud := prov.LastUserData()
	if sess.TunnelToken == "" || !strings.Contains(ud, sess.TunnelToken) {
		t.Fatalf("cloud-init UserData does not carry the session tunnel token")
	}
	if !strings.Contains(ud, addr) {
		t.Fatalf("cloud-init UserData does not carry the control-plane address %q:\n%s", addr, ud)
	}

	// The instance must NOT have been torn down on the happy path.
	if prov.DeleteCount() != 0 {
		t.Fatalf("DeleteCount = %d, want 0 (instance must not be torn down when ready)", prov.DeleteCount())
	}
	if len(prov.Instances()) != 1 {
		t.Fatalf("live instances = %d, want 1", len(prov.Instances()))
	}
}

// TestProvisionTeardownOnCreateFailure verifies that when CreateInstance fails,
// the session ends up `error` and the flow does not wedge. Since create failed,
// there is nothing to tear down.
func TestProvisionTeardownOnCreateFailure(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	prov.CreateErr = io.ErrUnexpectedEOF // any error

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: 2 * time.Second})

	cr := createSession(t, ts)
	if cr.Status != statusProvisioning {
		t.Fatalf("POST /sessions status = %q, want provisioning", cr.Status)
	}

	// The background flow marks the session error (the key assertion: the flow
	// didn't wedge and ends in a terminal-ish error state).
	waitForStatus(t, st, cr.SessionID, statusError, 3*time.Second)

	// Create failed (the fake records only successful creates), so there is no
	// instance and nothing was torn down — no leak is possible.
	if prov.CreateCount() != 0 {
		t.Fatalf("CreateCount = %d, want 0 (the forced-failure create records nothing)", prov.CreateCount())
	}
	if prov.DeleteCount() != 0 {
		t.Fatalf("DeleteCount = %d, want 0 (create failed, nothing to tear down)", prov.DeleteCount())
	}
}

// TestProvisionTeardownOnReadinessTimeout is the headline "no leak" guarantee:
// CreateInstance succeeds but NO agent ever connects, so readiness times out;
// the session must go `error` AND the instance must be destroyed.
func TestProvisionTeardownOnReadinessTimeout(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()
	// No OnCreate: no agent connects, so the session never becomes ready.

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{
		ProvisionTimeout: 200 * time.Millisecond, // tiny, so the test is fast
	})

	cr := createSession(t, ts)

	// The session goes error after the timeout...
	waitForStatus(t, st, cr.SessionID, statusError, 3*time.Second)

	// ...and the instance was destroyed (the no-leak guarantee). The fake hands
	// out fake-1 for the first create, and provisionSession persisted that id on
	// the session before waiting, so DELETE-on-timeout can find it.
	const wantID = "fake-1"
	if !prov.Deleted(wantID) {
		t.Fatalf("instance %s was NOT destroyed on readiness timeout (leak!); deleted=%v", wantID, prov.DeleteCount())
	}
	if prov.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", prov.CreateCount())
	}
}

// TestDeleteDestroysInstance verifies DELETE /sessions/{id} destroys the
// backing instance and marks the session terminated, idempotently. We inject a
// ready session row carrying an instance id directly (no need to drive a full
// provision) to isolate the DELETE behavior.
func TestDeleteDestroysInstance(t *testing.T) {
	st := newTestStore(t)
	prov := fake.New()

	// Pre-create an instance in the fake so its id is "live", then record it on
	// a session row.
	inst, err := prov.CreateInstance(context.Background(), provider.InstanceSpec{})
	if err != nil {
		t.Fatalf("seed CreateInstance: %v", err)
	}
	id, _ := store.NewSessionID()
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusReady, ToolEndpoint: "",
		Width: 1280, Height: 800, InstanceID: inst.ID,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	ts, _ := startCPWithProvider(t, st, prov, ProvisionConfig{ProvisionTimeout: time.Second})

	// First DELETE: terminates and destroys the instance.
	d1 := deleteSession(t, ts, id)
	if d1 != http.StatusOK {
		t.Fatalf("first DELETE status = %d, want 200", d1)
	}
	if !prov.Deleted(inst.ID) {
		t.Fatalf("instance %s was not destroyed by DELETE", inst.ID)
	}
	sess, _, _ := st.GetSession(id)
	if sess.Status != statusTerminated {
		t.Fatalf("status after DELETE = %q, want terminated", sess.Status)
	}
	deletesAfterFirst := prov.DeleteCount()

	// Second DELETE: still terminated (idempotent); DeleteInstance tolerates the
	// repeat (the fake records it but it is a no-op semantically).
	d2 := deleteSession(t, ts, id)
	if d2 != http.StatusOK {
		t.Fatalf("second DELETE status = %d, want 200 (idempotent)", d2)
	}
	sess2, _, _ := st.GetSession(id)
	if sess2.Status != statusTerminated {
		t.Fatalf("status after second DELETE = %q, want terminated", sess2.Status)
	}
	if prov.DeleteCount() < deletesAfterFirst {
		t.Fatalf("DeleteCount went backwards: %d < %d", prov.DeleteCount(), deletesAfterFirst)
	}
}

// deleteSession issues DELETE /sessions/{id} and returns the status code.
func deleteSession(t *testing.T, ts *httptest.Server, id string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sessions/"+id, nil)
	req.Header.Set("Authorization", "Bearer k_active")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE /sessions/%s: %v", id, err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}
