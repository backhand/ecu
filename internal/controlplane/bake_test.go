package controlplane

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/provider"
	"github.com/backhand/ecu/internal/provider/fake"
	"github.com/backhand/ecu/internal/store"
)

// callbackInBakeUserData extracts the bake-completion callback URL from a
// rendered bake cloud-init (the baker embeds the per-bake token in it).
var callbackInBakeUserData = regexp.MustCompile(`/internal/bake/(b_[0-9a-f]+)/done`)

// startBakeServer boots the control-plane Handler on a real httptest.Server
// wired with the fake provider and the given BakeConfig, returning the server,
// the *Server (so tests can read ActiveBootImage), and the host:port. The
// CallbackBaseURL is defaulted to the test server's own http:// address so the
// simulated bake instance can fire the real callback endpoint.
func startBakeServer(t *testing.T, prov provider.Provider, bc BakeConfig) (*httptest.Server, *Server, string) {
	t.Helper()
	st := newTestStore(t)
	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	if bc.CallbackBaseURL == "" {
		bc.CallbackBaseURL = "http://" + addr
	}
	if bc.BaseImage == "" {
		bc.BaseImage = "ubuntu-24.04"
	}
	srv := NewServer(st, "",
		WithListenAddr(addr),
		WithProvider(prov),
		// The provisioning flow reads ActiveBootImage, which falls back to
		// provisionCfg.BaseImage; keep it consistent with the bake base image.
		WithProvisionConfig(ProvisionConfig{BaseImage: bc.BaseImage, ProvisionTimeout: time.Second}),
		WithBakeConfig(bc),
	)
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, srv, addr
}

// TestStartBakeUsesExistingSnapshot is the fast path: FindImage hits, so the
// active boot image is set to the snapshot immediately and NO bake instance is
// created.
func TestStartBakeUsesExistingSnapshot(t *testing.T) {
	prov := fake.New()
	prov.FindImageResult = &fake.FindImageResult{
		Image: provider.Image{ID: "snap-9001", Name: "ecu-snap"},
		Found: true,
	}
	_, srv, _ := startBakeServer(t, prov, BakeConfig{
		ImageName:      "ecu-snap",
		ContainerImage: "ghcr.io/backhand/ecu-image:latest",
		Timeout:        2 * time.Second,
	})

	srv.StartBake(context.Background())

	// The snapshot's ID (not its name) becomes the active boot image — this is
	// what a Hetzner snapshot boot requires.
	if got := srv.ActiveBootImage(); got != "snap-9001" {
		t.Fatalf("ActiveBootImage = %q, want snap-9001 (the snapshot ID)", got)
	}
	// No bake instance was created.
	if prov.CreateCount() != 0 {
		t.Fatalf("CreateCount = %d, want 0 (existing snapshot -> no bake)", prov.CreateCount())
	}
	if prov.ImageCount() != 0 {
		t.Fatalf("ImageCount = %d, want 0 (no new snapshot baked)", prov.ImageCount())
	}
}

// TestBakeFlowCallbackThenSnapshot is the headline bake: FindImage misses, so a
// bake instance is created with the BAKE cloud-init; the simulated instance
// fires the outbound callback; the control plane then snapshots the temp
// instance, deletes it, and sets the snapshot as the active boot image.
func TestBakeFlowCallbackThenSnapshot(t *testing.T) {
	prov := fake.New()
	// FindImageResult nil -> not found -> bake.

	var ts *httptest.Server
	// On create, simulate the bake instance: parse the callback URL out of the
	// bake cloud-init and POST it (the outbound completion callback).
	prov.OnCreate = func(spec provider.InstanceSpec) {
		// Sanity: bake instance is labeled for orphan cleanup, boots the base OS
		// image, and its cloud-init pulls (not runs) the container image.
		if spec.Labels[bakeInstanceLabelKey] != bakeInstanceLabelValue {
			t.Errorf("bake instance missing label %s=%s: %v", bakeInstanceLabelKey, bakeInstanceLabelValue, spec.Labels)
		}
		m := callbackInBakeUserData.FindStringSubmatch(spec.UserData)
		if len(m) != 2 {
			t.Errorf("no bake callback URL in cloud-init:\n%s", spec.UserData)
			return
		}
		go func() {
			resp, err := ts.Client().Post(ts.URL+"/internal/bake/"+m[1]+"/done", "", nil)
			if err != nil {
				t.Errorf("fire bake callback: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
	}

	var srv *Server
	ts, srv, _ = startBakeServer(t, prov, BakeConfig{
		ImageName:      "ecu-snap",
		ContainerImage: "ghcr.io/backhand/ecu-image:latest",
		Timeout:        5 * time.Second,
	})

	// Run the bake synchronously here (StartBake would background it; we call
	// runBake directly so the test can await its completion deterministically).
	done := make(chan struct{})
	go func() { defer close(done); srv.runBake(context.Background()) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bake did not complete within 5s")
	}

	// A snapshot was created from the bake instance, the temp instance was torn
	// down, and the active boot image is now the snapshot's ID.
	if prov.ImageCount() != 1 {
		t.Fatalf("ImageCount = %d, want 1 (snapshot created)", prov.ImageCount())
	}
	img := prov.Images()[0]
	if img.Name != "ecu-snap" {
		t.Fatalf("snapshot name = %q, want ecu-snap", img.Name)
	}
	if img.FromInstance != "fake-1" {
		t.Fatalf("snapshot taken from %q, want fake-1 (the bake instance)", img.FromInstance)
	}
	if !prov.Deleted("fake-1") {
		t.Fatalf("bake instance fake-1 was NOT torn down (leak!)")
	}
	if len(prov.Instances()) != 0 {
		t.Fatalf("live instances = %d, want 0 (bake instance destroyed)", len(prov.Instances()))
	}
	// The fake's CreateImage returns id "fake-image-<name>"; that ID is what the
	// provisioner boots from.
	if got := srv.ActiveBootImage(); got != "fake-image-ecu-snap" {
		t.Fatalf("ActiveBootImage = %q, want fake-image-ecu-snap (the snapshot ID)", got)
	}
}

// TestBakeTimeoutTearsDownInstance is the no-leak guarantee: the bake instance
// is created but NO callback ever fires, so the bake times out; the temp
// instance MUST be destroyed and the active boot image MUST remain the base OS
// image (cold-boot fallback intact).
func TestBakeTimeoutTearsDownInstance(t *testing.T) {
	prov := fake.New()
	// No OnCreate: the simulated instance never calls back.

	_, srv, _ := startBakeServer(t, prov, BakeConfig{
		ImageName:      "ecu-snap",
		ContainerImage: "ghcr.io/backhand/ecu-image:latest",
		BaseImage:      "ubuntu-24.04",
		Timeout:        150 * time.Millisecond, // tiny so the test is fast
	})

	done := make(chan struct{})
	go func() { defer close(done); srv.runBake(context.Background()) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bake did not time out within 3s")
	}

	// The bake instance was created then destroyed (no leak), no snapshot taken.
	if prov.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", prov.CreateCount())
	}
	if !prov.Deleted("fake-1") {
		t.Fatalf("bake instance fake-1 was NOT destroyed on timeout (leak!)")
	}
	if prov.ImageCount() != 0 {
		t.Fatalf("ImageCount = %d, want 0 (no callback -> no snapshot)", prov.ImageCount())
	}
	// Cold-boot fallback intact: the active boot image is still the base OS image.
	if got := srv.ActiveBootImage(); got != "ubuntu-24.04" {
		t.Fatalf("ActiveBootImage = %q, want ubuntu-24.04 (cold-boot fallback after a failed bake)", got)
	}
}

// TestBakeSnapshotFailureTearsDownInstance verifies that if the snapshot step
// fails after a successful callback, the temp instance is still torn down and
// the boot image stays the base image.
func TestBakeSnapshotFailureTearsDownInstance(t *testing.T) {
	prov := fake.New()
	prov.CreateImageErr = io.ErrUnexpectedEOF // snapshot fails

	var ts *httptest.Server
	prov.OnCreate = func(spec provider.InstanceSpec) {
		m := callbackInBakeUserData.FindStringSubmatch(spec.UserData)
		if len(m) != 2 {
			t.Errorf("no bake callback URL in cloud-init:\n%s", spec.UserData)
			return
		}
		go func() {
			resp, err := ts.Client().Post(ts.URL+"/internal/bake/"+m[1]+"/done", "", nil)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}

	var srv *Server
	ts, srv, _ = startBakeServer(t, prov, BakeConfig{
		ImageName: "ecu-snap", ContainerImage: "img", BaseImage: "ubuntu-24.04", Timeout: 5 * time.Second,
	})

	done := make(chan struct{})
	go func() { defer close(done); srv.runBake(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bake did not finish within 5s")
	}

	if !prov.Deleted("fake-1") {
		t.Fatalf("bake instance fake-1 was NOT torn down after snapshot failure (leak!)")
	}
	if got := srv.ActiveBootImage(); got != "ubuntu-24.04" {
		t.Fatalf("ActiveBootImage = %q, want ubuntu-24.04 (boot image unchanged on snapshot failure)", got)
	}
}

// TestBakeOrphanCleanupOnStartup verifies StartBake reaps a leftover ecu-bake
// instance (from a crashed previous run) before re-baking. We pre-seed a live
// ecu-bake-labeled instance, set FindImage to hit (so StartBake stops after the
// fast path), and assert the orphan was destroyed.
func TestBakeOrphanCleanupOnStartup(t *testing.T) {
	prov := fake.New()
	// Leftover bake instance from a "previous crashed run".
	orphan, _ := prov.CreateInstance(context.Background(), provider.InstanceSpec{
		Labels: map[string]string{bakeInstanceLabelKey: bakeInstanceLabelValue},
	})
	// Snapshot already exists, so StartBake takes the fast path after cleanup.
	prov.FindImageResult = &fake.FindImageResult{Image: provider.Image{ID: "snap-1", Name: "ecu-snap"}, Found: true}

	_, srv, _ := startBakeServer(t, prov, BakeConfig{ImageName: "ecu-snap", ContainerImage: "img", Timeout: time.Second})
	srv.StartBake(context.Background())

	if !prov.Deleted(orphan.ID) {
		t.Fatalf("leftover bake instance %s was NOT cleaned up on startup (leak!)", orphan.ID)
	}
}

// TestBakeCallbackRejectsBadToken verifies the callback endpoint rejects an
// unknown token (404) and is EXEMPT from the API-key middleware (no
// Authorization header is required — a missing key must NOT yield 401, and a
// bad token must NOT yield 401 either; it is 404 "unknown bake").
func TestBakeCallbackRejectsBadToken(t *testing.T) {
	prov := fake.New()
	ts, srv, _ := startBakeServer(t, prov, BakeConfig{ImageName: "ecu-snap", ContainerImage: "img", Timeout: time.Second})

	// Register a real bake token so a valid one exists to contrast against.
	good := srv.bakeRegistry.register("b_goodtoken")
	defer srv.bakeRegistry.unregister("b_goodtoken")

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"unknown token", "b_does_not_exist", http.StatusNotFound},
		{"empty-ish token segment", "x", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No Authorization header on purpose: proves API-key exemption.
			resp, err := ts.Client().Post(ts.URL+"/internal/bake/"+tc.token+"/done", "", nil)
			if err != nil {
				t.Fatalf("POST callback: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (must be %d, NOT 401 — endpoint is API-key-exempt, token-checked)",
					resp.StatusCode, tc.want, tc.want)
			}
		})
	}

	// A bad token must NOT have fired the good channel.
	select {
	case <-good:
		t.Fatal("a bad-token callback fired the registered bake channel")
	default:
	}

	// The valid token fires the channel and returns 200.
	resp, err := ts.Client().Post(ts.URL+"/internal/bake/b_goodtoken/done", "", nil)
	if err != nil {
		t.Fatalf("POST valid callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", resp.StatusCode)
	}
	select {
	case <-good:
		// fired, as expected
	case <-time.After(time.Second):
		t.Fatal("valid-token callback did not fire the registered bake channel")
	}
}

// TestSessionsBeforeAndAfterBakeUseRightImage proves the active-boot-image
// switch: a session provisioned BEFORE a bake completes boots from the base OS
// image, and one provisioned AFTER boots from the snapshot. We assert via the
// BaseImage the fake captures on each CreateInstance.
func TestSessionsBeforeAndAfterBakeUseRightImage(t *testing.T) {
	prov := fake.New()
	st := newTestStore(t)
	srv := NewServer(st, "",
		WithProvider(prov),
		WithProvisionConfig(ProvisionConfig{
			BaseImage:        "ubuntu-24.04",
			TunnelURL:        "ws://127.0.0.1:0/agent/connect",
			AgentBinaryURL:   "https://example.invalid/ecu",
			ProvisionTimeout: 150 * time.Millisecond, // times out fast (no agent)
		}),
	)

	// BEFORE: no bake has completed -> ActiveBootImage falls back to the base OS
	// image, so a provisioned session boots from it.
	if got := srv.ActiveBootImage(); got != "ubuntu-24.04" {
		t.Fatalf("ActiveBootImage before bake = %q, want ubuntu-24.04", got)
	}
	id1 := provisionOnce(t, srv, st)
	specBefore := findSpecForSession(t, prov, id1)
	if specBefore.BaseImage != "ubuntu-24.04" {
		t.Fatalf("session-before-bake BaseImage = %q, want ubuntu-24.04 (cold boot)", specBefore.BaseImage)
	}

	// Simulate a completed bake by flipping the active boot image to the snapshot.
	srv.setActiveBootImage("snap-555")

	// AFTER: a newly provisioned session boots from the snapshot.
	id2 := provisionOnce(t, srv, st)
	specAfter := findSpecForSession(t, prov, id2)
	if specAfter.BaseImage != "snap-555" {
		t.Fatalf("session-after-bake BaseImage = %q, want snap-555 (boots from snapshot)", specAfter.BaseImage)
	}
}

// provisionOnce drives a single provision: it inserts a provisioning session row
// (with a tunnel token) and runs provisionSession SYNCHRONOUSLY. With no agent
// connecting it will time out and tear the instance down, but CreateInstance is
// called first and the fake captures the spec (including BaseImage), which is
// what the test inspects. Returns the session id.
func provisionOnce(t *testing.T, srv *Server, st *store.Store) string {
	t.Helper()
	id, err := store.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	tok := id + "_tok"
	if err := st.CreateSession(&store.Session{
		ID: id, Account: "admin", Status: statusProvisioning, Width: 1280, Height: 800,
		TunnelToken: tok,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	srv.provisionSession(id, tok)
	return id
}

// findSpecForSession returns the CreateInstance spec the fake captured for the
// instance named after sessionID.
func findSpecForSession(t *testing.T, prov *fake.Provider, sessionID string) provider.InstanceSpec {
	t.Helper()
	want := instanceName(sessionID)
	for _, s := range prov.Creates() {
		if s.Name == want {
			return s
		}
	}
	t.Fatalf("no CreateInstance spec found for session %s (instance %s)", sessionID, want)
	return provider.InstanceSpec{}
}
