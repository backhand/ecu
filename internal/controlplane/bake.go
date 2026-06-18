package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/backhand/ecu/internal/provider"
)

// bakeInstanceLabelKey/Value tag the temporary bake instance so startup
// orphan-cleanup can reap one leaked by a crashed previous run. They are
// provider labels (the hcloud impl always also adds ecu=managed).
const (
	bakeInstanceLabelKey   = "ecu-bake"
	bakeInstanceLabelValue = "1"
)

// bakeCallbackPrefix is the path prefix of the bake-completion callback. The
// full route is bakeCallbackPrefix + "{token}/done"; the per-bake token in the
// path authenticates it. Like agentConnectPath it is EXEMPT from the API-key
// middleware (a bake instance has no API key) and is instead authenticated by
// the token (constant-time compare). See authMiddleware and Handler.
const bakeCallbackPrefix = "/internal/bake/"

// BakeConfig carries everything the C7 pre-bake flow needs. It is supplied via
// WithBakeConfig from cmd/ecu (derived from the loaded config) and is only used
// when StartBake is invoked (i.e. when ECU_IMAGE is set).
type BakeConfig struct {
	// ImageName is the pre-baked SNAPSHOT NAME (ECU_IMAGE): the name the snapshot
	// is looked up / created under. NOT a container image ref.
	ImageName string

	// ContainerImage is the container (Docker) image ref the bake instance pulls
	// (ECU_CONTAINER_IMAGE), e.g. "ghcr.io/backhand/ecu-image:latest".
	ContainerImage string

	// CallbackBaseURL is the publicly reachable control-plane base URL the bake
	// instance curls when the pull finishes, e.g. "https://ecu.example.com". The
	// per-bake token + "/internal/bake/<token>/done" is appended. It is derived
	// like the agent tunnel URL (and shares the public-reachability dependency on
	// C10): a real cloud instance must be able to reach it.
	CallbackBaseURL string

	// InstanceType / Region / BaseImage configure the bake instance. BaseImage is
	// the base OS image (NOT the snapshot — the bake produces the snapshot).
	InstanceType string
	Region       string
	BaseImage    string

	// Timeout bounds the wait for the bake callback before tearing the bake
	// instance down (ECU_BAKE_TIMEOUT). Must be > 0 on the real path; tests
	// inject a short value.
	Timeout time.Duration
}

// bakeRegistry maps a per-bake token to the channel its callback fires. Only
// one bake runs at a time in practice, but the registry keeps the token check
// data-race free and lets the (token-authed) callback handler resolve a fire
// target without reaching into the baker goroutine.
type bakeRegistry struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func newBakeRegistry() *bakeRegistry {
	return &bakeRegistry{m: make(map[string]chan struct{})}
}

// register associates token with a freshly-made done channel and returns it.
func (r *bakeRegistry) register(token string) chan struct{} {
	ch := make(chan struct{})
	r.mu.Lock()
	r.m[token] = ch
	r.mu.Unlock()
	return ch
}

// unregister removes a token (called when the bake finishes/aborts so a late
// callback for a done bake is treated as unknown).
func (r *bakeRegistry) unregister(token string) {
	r.mu.Lock()
	delete(r.m, token)
	r.mu.Unlock()
}

// lookup resolves a presented token to its done channel using a constant-time
// compare against every registered token, so a wrong token cannot be
// distinguished by timing and an empty token never matches.
func (r *bakeRegistry) lookup(token string) (chan struct{}, bool) {
	if token == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for tok, ch := range r.m {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			return ch, true
		}
	}
	return nil, false
}

// ActiveBootImage returns the image reference new sessions currently boot from:
// the pre-baked snapshot once a bake completes (or a pre-existing one is found),
// otherwise the base OS image. The provisioning flow reads this when building
// InstanceSpec.BaseImage, so sessions BEFORE a bake completes cold-boot from the
// OS image and sessions AFTER boot from the snapshot — with no other
// provisioning change. It is safe for concurrent use.
func (s *Server) ActiveBootImage() string {
	s.bootImageMu.RLock()
	defer s.bootImageMu.RUnlock()
	if s.activeBootImage != "" {
		return s.activeBootImage
	}
	// Fall back to the configured base image. provisionCfg.BaseImage is the
	// cold-boot default; if even that is empty the provider applies its own.
	return s.provisionCfg.BaseImage
}

// setActiveBootImage updates the image new sessions boot from (the snapshot's
// reference once a bake completes or a pre-existing snapshot is found). For
// Hetzner this MUST be the snapshot's numeric ID, not its name (see the hcloud
// imageRef footgun): a snapshot is nameless and is booted by id.
func (s *Server) setActiveBootImage(ref string) {
	s.bootImageMu.Lock()
	s.activeBootImage = ref
	s.bootImageMu.Unlock()
	log.Printf("ecu bake: active boot image set to %q; new sessions will boot from it", ref)
}

// StartBake implements the C7 pre-bake decision and, when needed, runs the bake
// in the BACKGROUND so the control plane is usable immediately (sessions
// requested during a bake fall back to cold boot from the base OS image).
//
// Flow (see the brief):
//
//  1. Best-effort orphan cleanup: delete any leftover ecu-bake instance from a
//     crashed previous run BEFORE re-baking, so a restart mid-bake never leaks.
//  2. FindImage(ImageName). If found, immediately set it the active boot image
//     and return — no bake. (Fast path: snapshot already exists.)
//  3. If not found, launch the background baker and return. The baker boots a
//     temp instance with the BAKE cloud-init, waits for the outbound callback
//     (or Timeout), snapshots it via CreateImage, deletes the temp instance, and
//     sets the snapshot the active boot image. On ANY failure/timeout it deletes
//     the temp instance (never leak) and the system keeps working via cold boot.
//
// StartBake itself does the synchronous orphan-cleanup + FindImage and only the
// bake runs in the background. ctx governs the background baker's lifetime; main
// passes the server lifecycle context so shutdown aborts a bake (and the deferred
// teardown still fires).
func (s *Server) StartBake(ctx context.Context) {
	if s.provider == nil || s.bakeCfg.ImageName == "" {
		return // pre-baking not configured
	}

	// 1. Orphan cleanup (best-effort): reap a bake instance leaked by a crash.
	cleanupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	if n, err := s.provider.DeleteInstancesByLabel(cleanupCtx, bakeInstanceLabelKey, bakeInstanceLabelValue); err != nil {
		log.Printf("ecu bake: orphan cleanup: %v (continuing)", err)
	} else if n > 0 {
		log.Printf("ecu bake: orphan cleanup destroyed %d leftover bake instance(s)", n)
	}
	cancel()

	// 2. Fast path: snapshot already exists -> use it, no bake.
	findCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	img, found, err := s.provider.FindImage(findCtx, s.bakeCfg.ImageName)
	cancel()
	if err != nil {
		log.Printf("ecu bake: FindImage(%q): %v; sessions cold-boot until a bake succeeds", s.bakeCfg.ImageName, err)
		return
	}
	if found {
		log.Printf("ecu bake: found existing snapshot %q (id=%s); using it as the boot image (no bake)", img.Name, img.ID)
		s.setActiveBootImage(bootRefForImage(img))
		return
	}

	// 3. Not found: bake in the background.
	log.Printf("ecu bake: no snapshot named %q; starting background bake (sessions cold-boot until it completes)", s.bakeCfg.ImageName)
	go s.runBake(ctx)
}

// runBake performs the background bake: render the BAKE cloud-init, create the
// temp instance, await the callback (or timeout), snapshot, delete the temp
// instance, and set the snapshot the active boot image. The temp instance is
// torn down on EVERY exit path (success, timeout, error) so a bake never leaks a
// paid instance.
func (s *Server) runBake(ctx context.Context) {
	timeout := s.bakeCfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A per-bake token (crypto/rand) authenticates the outbound completion
	// callback. Register its done channel BEFORE creating the instance so a fast
	// callback can never race ahead of the registration.
	token, err := newBakeToken()
	if err != nil {
		log.Printf("ecu bake: generating bake token: %v; aborting bake (cold boot remains)", err)
		return
	}
	done := s.bakeRegistry.register(token)
	defer s.bakeRegistry.unregister(token)

	callbackURL := s.bakeCfg.CallbackBaseURL + bakeCallbackPrefix + token + "/done"
	userData, err := provider.RenderBakeCloudInit(provider.BakeCloudInitParams{
		ImageRef:    s.bakeCfg.ContainerImage,
		CallbackURL: callbackURL,
	})
	if err != nil {
		log.Printf("ecu bake: render bake cloud-init: %v; aborting bake", err)
		return
	}

	// Create the temp bake instance from the BASE OS image (it produces the
	// snapshot). Labeled ecu-bake so orphan cleanup can reap it after a crash.
	spec := provider.InstanceSpec{
		Name:      "ecu-bake",
		Type:      s.bakeCfg.InstanceType,
		Region:    s.bakeCfg.Region,
		BaseImage: s.bakeCfg.BaseImage,
		UserData:  userData,
		Labels:    map[string]string{bakeInstanceLabelKey: bakeInstanceLabelValue},
	}
	inst, err := s.provider.CreateInstance(ctx, spec)
	if err != nil {
		// CreateInstance may return an id alongside an error (server made, a
		// follow-up action failed) — tear it down so we never leak it.
		if inst.ID != "" {
			s.teardownBakeInstance(inst.ID)
		}
		log.Printf("ecu bake: create bake instance: %v; cold boot remains", err)
		return
	}
	// From here EVERY exit path must tear the bake instance down.
	defer s.teardownBakeInstance(inst.ID)
	log.Printf("ecu bake: bake instance %s created (ip=%s); awaiting image pull + callback (timeout %s)", inst.ID, inst.PublicIP, timeout)

	// Wait for the outbound completion callback or the timeout.
	select {
	case <-done:
		log.Printf("ecu bake: callback received for bake instance %s; snapshotting", inst.ID)
	case <-ctx.Done():
		log.Printf("ecu bake: bake did not complete within %s; tearing down bake instance %s (cold boot remains)", timeout, inst.ID)
		return
	}

	// Snapshot the bake instance into the named image, then (via the deferred
	// teardown) destroy the temp instance and mark the snapshot active. Use a
	// fresh bounded context so a tripped bake-timeout context cannot abort the
	// snapshot we just earned (the callback already arrived).
	snapCtx, snapCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer snapCancel()
	img, err := s.provider.CreateImage(snapCtx, inst.ID, s.bakeCfg.ImageName)
	if err != nil {
		log.Printf("ecu bake: CreateImage from %s: %v; cold boot remains", inst.ID, err)
		return
	}
	log.Printf("ecu bake: snapshot %q created (id=%s) from bake instance %s", img.Name, img.ID, inst.ID)
	s.setActiveBootImage(bootRefForImage(img))
}

// teardownBakeInstance destroys the temp bake instance, best-effort and
// idempotent (DeleteInstance tolerates an already-gone instance). A logged
// failure is an operational signal of a possible leak. A fresh bounded context
// is used so a cancelled parent context cannot prevent teardown.
func (s *Server) teardownBakeInstance(instanceID string) {
	if s.provider == nil || instanceID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.provider.DeleteInstance(ctx, instanceID); err != nil {
		log.Printf("ecu bake: FAILED to delete bake instance %s (possible leak): %v", instanceID, err)
		return
	}
	log.Printf("ecu bake: bake instance %s destroyed", instanceID)
}

// handleBakeDone is the bake-completion callback: <prefix>{token}/done. It is
// EXEMPT from the API-key middleware (a bake instance has no API key) and is
// authenticated solely by the per-bake token in the path, compared in constant
// time against the registered token. An unknown/expired/empty token yields 404
// with no effect (it does NOT reveal whether a bake is in flight). A valid token
// fires the baker's done channel exactly once (a duplicate callback after the
// bake finished is a harmless 404, since the token is unregistered on
// completion).
func (s *Server) handleBakeDone(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	ch, ok := s.bakeRegistry.lookup(token)
	if !ok {
		// Unknown/expired/empty token: no effect, indistinguishable to the caller.
		http.Error(w, "unknown bake", http.StatusNotFound)
		return
	}
	// Fire the channel exactly once; a second callback for the same token finds
	// it already closed and must not panic on a re-close. unregister on the
	// baker side makes the duplicate a 404, but guard the close regardless.
	select {
	case <-ch:
		// already fired
	default:
		close(ch)
	}
	w.WriteHeader(http.StatusOK)
}

// bootRefForImage returns the reference the provisioner should boot from for a
// pre-baked image. For Hetzner a snapshot is nameless and booted by numeric ID;
// the neutral rule is "prefer the ID" (which the hcloud CreateInstance treats as
// an image id via the all-digits check). The ID is always populated by both the
// hcloud FindImage/CreateImage and the fake.
func bootRefForImage(img provider.Image) string {
	if img.ID != "" {
		return img.ID
	}
	return img.Name
}

// newBakeToken returns a fresh per-bake token: "b_" + 64 hex chars (32 random
// bytes from crypto/rand). It authenticates the single outbound bake-completion
// callback and is compared in constant time, mirroring the tunnel-token design.
func newBakeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("controlplane: generating bake token: %w", err)
	}
	return "b_" + hex.EncodeToString(b), nil
}
