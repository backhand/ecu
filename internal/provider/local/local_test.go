package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/backhand/ecu/internal/provider"
)

// fakeRunner is a deterministic dockerRunner for tests: it returns canned
// stdout/err keyed on the first arg (the docker subcommand — "run", "inspect",
// "rm", "ps") and records every call's full args so tests can assert what was
// (or was not) invoked — in particular whether `docker rm -f` fired on a
// teardown path. No real docker is ever executed.
type fakeRunner struct {
	mu sync.Mutex

	// out / errs map a docker subcommand to the stdout / error it returns.
	out  map[string]string
	errs map[string]error

	// calls records every invocation's args in order.
	calls [][]string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{out: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if err := f.errs[sub]; err != nil {
		return "", err
	}
	return f.out[sub], nil
}

// ranSubcommand reports whether a docker call with the given first arg was made.
func (f *fakeRunner) ranSubcommand(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == sub {
			return true
		}
	}
	return false
}

// argsFor returns the recorded args of the FIRST call whose first arg is sub.
func (f *fakeRunner) argsFor(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == sub {
			return c
		}
	}
	return nil
}

// ranRmForce reports whether a `docker rm -f <id>` call was made (the teardown
// signal). It matches the rm subcommand carrying the -f flag.
func (f *fakeRunner) ranRmForce() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) >= 2 && c[0] == "rm" && c[1] == "-f" {
			return true
		}
	}
	return false
}

// portOf extracts the port out of an httptest.Server's URL (e.g. "57394"),
// which the fake inspect returns so endpoint+"/healthz" actually hits ts.
func portOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse httptest URL %q: %v", ts.URL, err)
	}
	return u.Port()
}

// TestCreateInstanceHappyPath: run returns a container id; inspect returns the
// port of a live httptest server whose /healthz returns 200; CreateInstance
// returns a running instance pointed at that endpoint and tears nothing down. It
// also asserts the docker run args carry the expected flags + defaults + image.
func TestCreateInstanceHappyPath(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer health.Close()
	port := portOf(t, health)

	r := newFakeRunner()
	r.out["run"] = "container-abc123"
	r.out["inspect"] = port

	p := newWithRunner(provider.Config{ContainerImage: "ecu-image:dev"}, r)
	p.healthInterval = 5 * time.Millisecond // fast poll for the test

	inst, err := p.CreateInstance(context.Background(), provider.InstanceSpec{})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.ID != "container-abc123" {
		t.Fatalf("inst.ID = %q, want container-abc123", inst.ID)
	}
	wantEndpoint := "http://127.0.0.1:" + port
	if inst.Endpoint != wantEndpoint {
		t.Fatalf("inst.Endpoint = %q, want %q", inst.Endpoint, wantEndpoint)
	}
	if inst.Status != "running" {
		t.Fatalf("inst.Status = %q, want running", inst.Status)
	}

	// Assert the docker run args.
	run := strings.Join(r.argsFor("run"), " ")
	for _, want := range []string{
		"-p 127.0.0.1::8000",
		"--platform linux/amd64",
		"WIDTH=1280", // default
		"HEIGHT=800", // default
		"ecu-image:dev",
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("docker run args %q missing %q", run, want)
		}
	}

	// Happy path: nothing torn down.
	if r.ranRmForce() {
		t.Fatalf("docker rm -f was called on the happy path; want no teardown")
	}
}

// TestCreateInstanceTeardownOnHealthTimeout: /healthz never returns 200, so the
// bounded health wait expires; CreateInstance must error AND tear the container
// down (docker rm -f <id>).
func TestCreateInstanceTeardownOnHealthTimeout(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // never healthy
	}))
	defer health.Close()
	port := portOf(t, health)

	r := newFakeRunner()
	r.out["run"] = "container-stuck"
	r.out["inspect"] = port

	p := newWithRunner(provider.Config{ContainerImage: "ecu-image:dev"}, r)
	p.healthTimeout = 150 * time.Millisecond
	p.healthInterval = 20 * time.Millisecond

	inst, err := p.CreateInstance(context.Background(), provider.InstanceSpec{})
	if err == nil {
		t.Fatalf("CreateInstance returned nil error, want a health-timeout error (inst=%+v)", inst)
	}
	if !r.ranRmForce() {
		t.Fatalf("docker rm -f was NOT called after a health timeout; the container leaked")
	}
}

// TestCreateInstanceNoImage: an unset ContainerImage errors early, before any
// docker call.
func TestCreateInstanceNoImage(t *testing.T) {
	r := newFakeRunner()
	p := newWithRunner(provider.Config{}, r)
	if _, err := p.CreateInstance(context.Background(), provider.InstanceSpec{}); err == nil {
		t.Fatalf("CreateInstance with no image returned nil error, want a configuration error")
	}
	if r.ranSubcommand("run") {
		t.Fatalf("docker run was called despite no configured image")
	}
}

// TestDeleteInstanceIdempotent: a "No such container" error is swallowed
// (returns nil), and an empty id is a no-op that never calls the runner.
func TestDeleteInstanceIdempotent(t *testing.T) {
	r := newFakeRunner()
	r.errs["rm"] = fmt.Errorf("docker rm -f x: exit status 1: Error response from daemon: No such container: x")

	p := newWithRunner(provider.Config{}, r)
	if err := p.DeleteInstance(context.Background(), "x"); err != nil {
		t.Fatalf("DeleteInstance with a No-such-container error = %v, want nil (idempotent)", err)
	}

	// Empty id: no runner call at all.
	r2 := newFakeRunner()
	p2 := newWithRunner(provider.Config{}, r2)
	if err := p2.DeleteInstance(context.Background(), ""); err != nil {
		t.Fatalf("DeleteInstance(\"\") = %v, want nil", err)
	}
	if len(r2.calls) != 0 {
		t.Fatalf("DeleteInstance(\"\") called the runner %d times, want 0", len(r2.calls))
	}
}

// TestDeleteInstancePropagatesRealError: a non-"No such container" error is
// returned (not swallowed).
func TestDeleteInstancePropagatesRealError(t *testing.T) {
	r := newFakeRunner()
	r.errs["rm"] = fmt.Errorf("docker rm -f y: exit status 1: Cannot connect to the Docker daemon")
	p := newWithRunner(provider.Config{}, r)
	if err := p.DeleteInstance(context.Background(), "y"); err == nil {
		t.Fatalf("DeleteInstance with a daemon error = nil, want the error propagated")
	}
}

// TestDeleteInstancesByLabel: ps lists two ids; each is removed and counted; an
// empty/blank line is skipped.
func TestDeleteInstancesByLabel(t *testing.T) {
	r := newFakeRunner()
	r.out["ps"] = "id-1\nid-2\n\n"
	p := newWithRunner(provider.Config{}, r)
	n, err := p.DeleteInstancesByLabel(context.Background(), managedLabelKey, managedLabelValue)
	if err != nil {
		t.Fatalf("DeleteInstancesByLabel: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteInstancesByLabel count = %d, want 2", n)
	}
	if !r.ranRmForce() {
		t.Fatalf("DeleteInstancesByLabel did not remove the matched containers")
	}
}

// TestDeleteInstancesByLabelEmpty: matching nothing returns (0, nil).
func TestDeleteInstancesByLabelEmpty(t *testing.T) {
	r := newFakeRunner()
	r.out["ps"] = ""
	p := newWithRunner(provider.Config{}, r)
	n, err := p.DeleteInstancesByLabel(context.Background(), "k", "v")
	if err != nil {
		t.Fatalf("DeleteInstancesByLabel: %v", err)
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
}

// TestEnsureFirewallNil: the local provider has no firewall concept.
func TestEnsureFirewallNil(t *testing.T) {
	p := newWithRunner(provider.Config{}, newFakeRunner())
	if err := p.EnsureFirewall(context.Background(), nil); err != nil {
		t.Fatalf("EnsureFirewall = %v, want nil", err)
	}
}

// TestCreateImageUnsupported: image snapshots are not supported locally.
func TestCreateImageUnsupported(t *testing.T) {
	p := newWithRunner(provider.Config{}, newFakeRunner())
	if _, err := p.CreateImage(context.Background(), "inst", "name"); err == nil {
		t.Fatalf("CreateImage = nil error, want unsupported error")
	}
}

// TestRequiresCloudInitFalse: the local provider is reached directly, not via
// cloud-init / tunnel.
func TestRequiresCloudInitFalse(t *testing.T) {
	p := newWithRunner(provider.Config{}, newFakeRunner())
	if p.RequiresCloudInit() {
		t.Fatalf("RequiresCloudInit() = true, want false for the local provider")
	}
}

// TestDeleteImageNil: a no-op idempotent delete.
func TestDeleteImageNil(t *testing.T) {
	p := newWithRunner(provider.Config{}, newFakeRunner())
	if err := p.DeleteImage(context.Background(), "anything"); err != nil {
		t.Fatalf("DeleteImage = %v, want nil", err)
	}
}

// TestFindImageAbsent: no local snapshots, so found=false with no error.
func TestFindImageAbsent(t *testing.T) {
	p := newWithRunner(provider.Config{}, newFakeRunner())
	img, found, err := p.FindImage(context.Background(), "name")
	if err != nil {
		t.Fatalf("FindImage err = %v, want nil", err)
	}
	if found {
		t.Fatalf("FindImage found = true, want false (img=%+v)", img)
	}
}
