package controlplane

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/backhand/ecu/internal/provider"
)

// provisionPollInterval is how often the readiness waiter polls the store for
// the session flipping to ready (the broker flips it when the agent's tunnel
// registers).
const provisionPollInterval = 200 * time.Millisecond

// provisionSession runs the production provisioning flow for an
// already-persisted provisioning session: render cloud-init, create the
// instance, then wait for the agent to register (readiness) within the
// provision timeout. On ANY failure after the instance exists — create-action
// failure, readiness timeout, or a mid-flight error — it tears the instance
// down (best-effort) and marks the session error. A leaked paid instance is
// unacceptable, so every post-create exit path destroys the instance.
//
// This runs in a BACKGROUND goroutine (see handleCreateSession): POST /sessions
// returns `provisioning` immediately per the API contract, and this observes
// the store independently. It therefore MUST use a context derived from
// context.Background() (NOT the request context, which ends when the handler
// returns) bounded by the provision timeout.
//
// Correctness under the concurrent broker:
//   - The agent may connect and flip the session to ready at any time; the
//     waiter just observes the store, so reaching ready is detected and
//     teardown does NOT fire.
//   - If the session is DELETEd while provisioning (status terminated), the
//     waiter stops and does NOT resurrect or override it.
func (s *Server) provisionSession(sessionID, tunnelToken string) {
	timeout := s.provisionCfg.ProvisionTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 1. Render cloud-init carrying THIS session's tunnel token and the control
	// plane's public tunnel URL. A render error means we never created an
	// instance, so there is nothing to tear down.
	userData, err := provider.RenderCloudInit(provider.CloudInitParams{
		ControlPlaneURL: s.provisionCfg.TunnelURL,
		TunnelToken:     tunnelToken,
		ImageRef:        s.provisionCfg.ImageRef,
		Width:           s.provisionCfg.Width,
		Height:          s.provisionCfg.Height,
		AgentBinaryURL:  s.provisionCfg.AgentBinaryURL,
	})
	if err != nil {
		log.Printf("ecu provision: session %s: render cloud-init: %v", sessionID, err)
		s.markErrorIfNotTerminated(sessionID)
		return
	}

	// 2. Create the instance. BaseImage is read from ActiveBootImage so a session
	// provisioned BEFORE a pre-bake completes cold-boots from the OS image and one
	// AFTER boots from the snapshot — the single provisioning hook for C7. On a
	// baked instance the session cloud-init's `docker run` finds the image locally
	// and skips the pull.
	spec := provider.InstanceSpec{
		Name:      instanceName(sessionID),
		Type:      s.provisionCfg.InstanceType,
		Region:    s.provisionCfg.Region,
		BaseImage: s.ActiveBootImage(),
		UserData:  userData,
		Labels:    map[string]string{"ecu-session": sessionID},
	}
	inst, err := s.provider.CreateInstance(ctx, spec)
	if err != nil {
		// CreateInstance may still return an instance id alongside an error if
		// the server was created but a follow-up action failed — tear it down so
		// we never leak it.
		if inst.ID != "" {
			s.teardownInstance(sessionID, inst.ID)
		}
		log.Printf("ecu provision: session %s: create instance: %v", sessionID, err)
		s.markErrorIfNotTerminated(sessionID)
		return
	}

	// 3. Persist the instance id so DELETE (and any later teardown) can find it.
	if err := s.store.UpdateSessionInstanceID(sessionID, inst.ID); err != nil {
		log.Printf("ecu provision: session %s: persist instance id %s: %v", sessionID, inst.ID, err)
		// We have a live instance but couldn't record its id; tear it down to
		// avoid an untracked leak.
		s.teardownInstance(sessionID, inst.ID)
		s.markErrorIfNotTerminated(sessionID)
		return
	}
	log.Printf("ecu provision: session %s: instance %s created (ip=%s); awaiting agent", sessionID, inst.ID, inst.PublicIP)

	// 4. Wait for readiness (the broker flips status to ready when the agent
	// registers). On timeout, terminated, or store error -> teardown path.
	switch s.waitForReady(ctx, sessionID) {
	case readyReached:
		log.Printf("ecu provision: session %s ready on instance %s", sessionID, inst.ID)
	case readyTerminated:
		// Session was DELETEd while provisioning. DELETE owns teardown of the
		// instance; do not override the terminated status here. (DELETE reads
		// the instance id we persisted above.)
		log.Printf("ecu provision: session %s terminated during provisioning; waiter stopping", sessionID)
	default: // readyTimeout
		log.Printf("ecu provision: session %s did not become ready within %s; tearing down instance %s", sessionID, timeout, inst.ID)
		s.teardownInstance(sessionID, inst.ID)
		s.markErrorIfNotTerminated(sessionID)
	}
}

// readyResult enumerates the outcome of waitForReady.
type readyResult int

const (
	readyTimeout    readyResult = iota // deadline/ctx done before ready
	readyReached                       // session reached ready
	readyTerminated                    // session was terminated (DELETEd) while waiting
)

// waitForReady polls the store until the session reaches ready, is terminated,
// or ctx is done. It NEVER mutates the session; it only observes, so it races
// safely with the broker (which flips ready) and DELETE (which terminates).
func (s *Server) waitForReady(ctx context.Context, sessionID string) readyResult {
	ticker := time.NewTicker(provisionPollInterval)
	defer ticker.Stop()
	for {
		sess, found, err := s.store.GetSession(sessionID)
		if err == nil && found {
			switch sess.Status {
			case statusReady:
				return readyReached
			case statusTerminated:
				return readyTerminated
			}
		}
		select {
		case <-ctx.Done():
			// Re-check once on deadline so a ready flip that landed in the same
			// tick as the timeout is not misreported as a timeout (avoids a
			// needless teardown of a healthy instance).
			if sess, found, err := s.store.GetSession(sessionID); err == nil && found {
				switch sess.Status {
				case statusReady:
					return readyReached
				case statusTerminated:
					return readyTerminated
				}
			}
			return readyTimeout
		case <-ticker.C:
		}
	}
}

// teardownInstance destroys the provider instance backing a session,
// best-effort. DeleteInstance is idempotent, so this is safe to call even if
// the instance was already (or concurrently) destroyed. Failures are logged,
// not propagated — but a logged teardown failure is an operational signal that
// an instance may be leaking. Uses a fresh bounded context so a cancelled
// parent context cannot prevent teardown.
func (s *Server) teardownInstance(sessionID, instanceID string) {
	if s.provider == nil || instanceID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.provider.DeleteInstance(ctx, instanceID); err != nil {
		log.Printf("ecu provision: session %s: FAILED to delete instance %s (possible leak): %v", sessionID, instanceID, err)
		return
	}
	log.Printf("ecu provision: session %s: instance %s destroyed", sessionID, instanceID)
}

// markErrorIfNotTerminated flips the session to error unless it has already
// been terminated (e.g. a concurrent DELETE), which must not be overridden.
func (s *Server) markErrorIfNotTerminated(sessionID string) {
	cur, found, err := s.store.GetSession(sessionID)
	if err != nil || !found {
		return
	}
	if cur.Status == statusTerminated {
		return
	}
	if err := s.store.UpdateSessionStatus(sessionID, statusError); err != nil {
		log.Printf("ecu provision: session %s: mark error: %v", sessionID, err)
	}
}

// instanceName builds a provider instance name from a session id. Hetzner
// requires RFC-1123-ish names; the session id ("s_"+hex) becomes
// "ecu-<hex>" by replacing the underscore.
func instanceName(sessionID string) string {
	return fmt.Sprintf("ecu-%s", sanitizeName(sessionID))
}

// sanitizeName replaces characters not allowed in provider instance names
// (notably '_') with '-'.
func sanitizeName(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			// allowed
		default:
			b[i] = '-'
		}
	}
	return string(b)
}
