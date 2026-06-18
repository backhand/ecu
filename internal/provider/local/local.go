// Package local is the co-located implementation of provider.Provider: instead
// of provisioning a cloud instance, it runs each disposable desktop as a Docker
// container ON the control-plane box itself, reached over localhost. It is
// selected with ECU_PROVIDER=local.
//
// It is the one provider that does NOT use the reverse tunnel. A cloud instance
// boots, runs cloud-init to fetch the agent + container image, and dials OUT to
// the tunnel; here there is no instance and no agent — the container's
// tool-server port is published bound to 127.0.0.1, and the control plane talks
// to it directly at Instance.Endpoint (http://127.0.0.1:<port>). RequiresCloudInit
// therefore returns false, and CreateInstance waits for the container's /healthz
// itself (the cloud path's readiness wait is the agent registering the tunnel,
// which never happens locally).
//
// Persistence (snapshot-and-restore) is NOT supported locally — image snapshots
// are a cloud-instance concept — so CreateImage errors and the control plane
// gates persistent/restore requests off on this provider.
//
// Like the hcloud package it registers itself with the provider factory from
// init(), so a blank import of this package (done in cmd/ecu) makes
// provider.New("local", ...) work.
package local

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/backhand/ecu/internal/provider"
)

const (
	// containerToolPort is the in-container tool-server port published to a
	// localhost-only host port. It mirrors the cloud cloud-init's published port.
	containerToolPort = "8000"
	// managedLabelKey/Value tag every container this provider runs so
	// DeleteInstancesByLabel (orphan cleanup) can find them.
	managedLabelKey   = "ecu-managed"
	managedLabelValue = "1"
	// defaultWidth/defaultHeight are the desktop resolution used when Config
	// leaves Width/Height zero.
	defaultWidth  = 1280
	defaultHeight = 800
	// defaultHealthTimeout caps how long CreateInstance waits for a container's
	// /healthz to return 200 before tearing it down. A cold container takes
	// ~18s to come up, so this is generous; it is also bounded by the caller's
	// provision timeout (the smaller wins).
	defaultHealthTimeout = 90 * time.Second
	// defaultHealthInterval is how often the health wait polls /healthz.
	defaultHealthInterval = 1 * time.Second
	// healthGETTimeout bounds a single /healthz GET so a container that accepts
	// the connection but never responds cannot stall a whole poll interval.
	healthGETTimeout = 3 * time.Second
	// dockerVersionTimeout bounds the one-shot `docker version` availability
	// probe New runs at construction.
	dockerVersionTimeout = 10 * time.Second
)

// dockerRunner runs the `docker` CLI. It is an interface so unit tests can
// inject a fake that returns canned output and records the args, exercising the
// CreateInstance / DeleteInstance flows without a real Docker daemon.
type dockerRunner interface {
	// Run executes `docker <args...>` and returns trimmed stdout. On failure the
	// error includes stderr so messages are clear.
	Run(ctx context.Context, args ...string) (stdout string, err error)
}

// execRunner is the real dockerRunner: it shells out to the `docker` binary.
type execRunner struct{}

// Run executes `docker <args...>`, capturing stdout and stderr separately. On
// success it returns trimmed stdout; on failure it returns an error wrapping
// both the exec error and stderr so the message names what actually went wrong.
func (execRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Provider implements provider.Provider by running each session as a Docker
// container on the control-plane box.
type Provider struct {
	cfg    provider.Config
	runner dockerRunner

	// healthTimeout caps the per-CreateInstance health wait; healthInterval is
	// the poll cadence. Both are fields (not constants) so tests can shrink them.
	healthTimeout  time.Duration
	healthInterval time.Duration

	// httpClient performs the /healthz GETs with a short per-request timeout.
	httpClient *http.Client
}

func init() {
	provider.Register("local", func(cfg provider.Config) (provider.Provider, error) {
		return New(cfg)
	})
}

// New builds a local provider backed by the real `docker` CLI. It probes
// `docker version` up front so a misconfigured host (no daemon, no binary)
// fails clearly at startup rather than per-session at create time.
func New(cfg provider.Config) (*Provider, error) {
	p := newWithRunner(cfg, execRunner{})
	ctx, cancel := context.WithTimeout(context.Background(), dockerVersionTimeout)
	defer cancel()
	if _, err := p.runner.Run(ctx, "version"); err != nil {
		return nil, fmt.Errorf("local provider: docker is not available: %w", err)
	}
	return p, nil
}

// newWithRunner builds a local provider with an injected runner and the same
// defaults New uses, but WITHOUT the docker-version probe — so unit tests can
// drive it with a fake runner. Tests may then override healthTimeout /
// healthInterval on the returned *Provider to keep them fast.
func newWithRunner(cfg provider.Config, runner dockerRunner) *Provider {
	return &Provider{
		cfg:            cfg,
		runner:         runner,
		healthTimeout:  defaultHealthTimeout,
		healthInterval: defaultHealthInterval,
		httpClient:     &http.Client{Timeout: healthGETTimeout},
	}
}

// CreateInstance runs a container for the session, reads its published
// localhost port, waits for the container's /healthz to return 200, and returns
// the directly-reachable endpoint. spec is ignored beyond what the container
// needs — the image and resolution come from Config; there is no cloud-init,
// region, or base image locally. ANY failure after the container starts tears
// it down (best-effort) so a container never leaks.
func (p *Provider) CreateInstance(ctx context.Context, _ provider.InstanceSpec) (provider.Instance, error) {
	image := p.cfg.ContainerImage
	if image == "" {
		return provider.Instance{}, fmt.Errorf("local provider: no container image configured (set ECU_CONTAINER_IMAGE)")
	}
	w := p.cfg.Width
	if w <= 0 {
		w = defaultWidth
	}
	h := p.cfg.Height
	if h <= 0 {
		h = defaultHeight
	}

	// Start the container detached. -p 127.0.0.1::8000 publishes the tool-server
	// port to an OS-assigned host port bound to localhost ONLY (never the public
	// interface). docker run -d prints the container id.
	runArgs := []string{
		"run", "-d",
		"--platform", "linux/amd64",
		"-p", "127.0.0.1::" + containerToolPort,
		"-e", fmt.Sprintf("WIDTH=%d", w),
		"-e", fmt.Sprintf("HEIGHT=%d", h),
		"--label", managedLabelKey + "=" + managedLabelValue,
		image,
	}
	id, err := p.runner.Run(ctx, runArgs...)
	if err != nil {
		// No id was produced, so there is nothing to tear down.
		return provider.Instance{}, fmt.Errorf("local provider: docker run: %w", err)
	}

	// From here on every error path must tear the container down before
	// returning so a failed create never leaks a running container.

	// Read the localhost host port docker assigned to the container's tool port.
	port, err := p.runner.Run(ctx, "inspect",
		"--format", `{{ (index (index .NetworkSettings.Ports "8000/tcp") 0).HostPort }}`,
		id)
	if err != nil {
		p.removeContainer(context.Background(), id)
		return provider.Instance{}, fmt.Errorf("local provider: docker inspect port of %s: %w", id, err)
	}
	port = strings.TrimSpace(port)
	if port == "" {
		p.removeContainer(context.Background(), id)
		return provider.Instance{}, fmt.Errorf("local provider: container %s exposes no host port", id)
	}
	endpoint := "http://127.0.0.1:" + port

	// Wait for the tool server to answer /healthz with 200. Bound the wait by
	// the SMALLER of the provision context and healthTimeout so a slow container
	// is torn down rather than hung on.
	if err := p.waitHealthy(ctx, endpoint); err != nil {
		p.removeContainer(context.Background(), id)
		return provider.Instance{}, err
	}

	return provider.Instance{ID: id, Endpoint: endpoint, Status: "running"}, nil
}

// waitHealthy polls endpoint+"/healthz" until it returns HTTP 200 or the wait
// is exhausted. The wait is bounded by min(ctx deadline, healthTimeout). It
// returns a clear error naming the container and timeout when health is never
// reached.
func (p *Provider) waitHealthy(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, p.healthTimeout)
	defer cancel()

	ticker := time.NewTicker(p.healthInterval)
	defer ticker.Stop()

	healthURL := endpoint + "/healthz"
	for {
		if p.healthOK(ctx, healthURL) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local provider: container at %s did not become healthy within %s", endpoint, p.healthTimeout)
		case <-ticker.C:
		}
	}
}

// healthOK performs a single GET of healthURL and reports whether it returned
// HTTP 200. The response body is always drained-and-closed. A request error
// (connection refused while the container is still starting) is simply "not yet
// healthy".
func (p *Provider) healthOK(ctx context.Context, healthURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// DeleteInstance destroys the container with the given id. It is idempotent: an
// empty id is a no-op, and a "No such container" response (the container was
// already removed) returns nil so teardown can be retried safely. Other errors
// propagate.
func (p *Provider) DeleteInstance(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if err := p.rm(ctx, id); err != nil {
		if isNoSuchContainer(err) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("local provider: docker rm %s: %w", id, err)
	}
	return nil
}

// DeleteInstancesByLabel destroys every managed container carrying the label
// key=value, returning the count destroyed. It is the orphan-cleanup primitive
// (best-effort, idempotent): matching nothing returns (0, nil); a list error
// returns (0, err). Per-container removals are best-effort (a container vanishing
// between the list and the remove is fine).
func (p *Provider) DeleteInstancesByLabel(ctx context.Context, key, value string) (int, error) {
	out, err := p.runner.Run(ctx, "ps", "-aq", "--filter", "label="+key+"="+value)
	if err != nil {
		return 0, fmt.Errorf("local provider: docker ps by label %s=%s: %w", key, value, err)
	}
	count := 0
	for _, id := range strings.Split(out, "\n") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		p.removeContainer(ctx, id)
		count++
	}
	return count, nil
}

// CreateImage is unsupported locally: image snapshots are a cloud-instance
// concept. Persistent sessions are gated off on this provider, so this is never
// reached in normal operation, but it returns a clear error rather than panic.
func (p *Provider) CreateImage(_ context.Context, _ /*fromInstance*/, _ /*name*/ string) (provider.Image, error) {
	return provider.Image{}, fmt.Errorf("local provider: image snapshots are not supported by the local provider")
}

// DeleteImage is a no-op locally (no snapshots exist), idempotent by definition.
func (p *Provider) DeleteImage(_ context.Context, _ string) error { return nil }

// FindImage always reports the image absent locally: there are no provider
// snapshots, so sessions always cold-run the container image. found=false with
// no error is the correct "no such snapshot" answer.
func (p *Provider) FindImage(_ context.Context, _ string) (provider.Image, bool, error) {
	return provider.Image{}, false, nil
}

// EnsureFirewall is a no-op locally. Every container's tool-server port is
// published bound to 127.0.0.1 only, so it is never reachable off-box — there
// is no cloud firewall to manage and nothing for a rule set to apply to.
func (p *Provider) EnsureFirewall(_ context.Context, _ []provider.FirewallRule) error { return nil }

// RequiresCloudInit reports false: the local provider runs the container
// directly and the control plane reaches it at Instance.Endpoint, so no
// cloud-init, agent, or tunnel is involved.
func (p *Provider) RequiresCloudInit() bool { return false }

// removeContainer is the best-effort teardown helper used on CreateInstance
// failure paths and by label cleanup: it removes the container and swallows ALL
// errors (including "No such container"). Teardown must never fail a caller, so
// nothing is returned.
func (p *Provider) removeContainer(ctx context.Context, id string) {
	if id == "" {
		return
	}
	_ = p.rm(ctx, id)
}

// rm is the shared low-level `docker rm -f <id>` call behind DeleteInstance
// (which inspects the error) and removeContainer (which swallows it).
func (p *Provider) rm(ctx context.Context, id string) error {
	_, err := p.runner.Run(ctx, "rm", "-f", id)
	return err
}

// isNoSuchContainer reports whether err is Docker's "No such container"
// response. `docker rm -f` of an unknown id prints that to stderr and the
// runner surfaces it in the error text; treating it as success keeps DeleteInstance
// idempotent.
func isNoSuchContainer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such container")
}
