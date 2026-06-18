// Package fake provides an in-memory provider.Provider for control-plane flow
// tests. It records calls (so teardown tests can assert "instance X was
// destroyed"), exposes the UserData passed to CreateInstance (so tests can
// assert the cloud-init carried the session's tunnel token), and offers hooks
// to force failures and to drive a session to ready on create.
//
// It is safe for concurrent use: the readiness-wait background goroutine and
// the agent-driving OnCreate hook touch it from different goroutines.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/backhand/ecu/internal/provider"
)

// Provider is an in-memory provider.Provider for tests.
type Provider struct {
	mu sync.Mutex

	// nextID is the incrementing instance-id counter (fake-1, fake-2, ...).
	nextID int

	// creates records each CreateInstance spec in call order.
	creates []provider.InstanceSpec
	// instances maps a returned instance id to its Instance.
	instances map[string]provider.Instance
	// deleted records DeleteInstance ids in call order (including repeats).
	deleted []string

	// images records CreateImage names; firewalls records EnsureFirewall calls.
	images    []string
	firewalls [][]provider.FirewallRule

	// CreateErr, when non-nil, makes CreateInstance fail with it (and record
	// nothing as created). Set before the call.
	CreateErr error

	// OnCreate, when set, is invoked (synchronously, inside CreateInstance,
	// after the instance is recorded and BEFORE CreateInstance returns) with
	// the spec. A test typically uses it to, in a goroutine, launch the real
	// agent against the test control plane using the token parsed out of
	// spec.UserData — driving the session to ready.
	OnCreate func(provider.InstanceSpec)
}

// New returns an empty fake provider.
func New() *Provider {
	return &Provider{instances: make(map[string]provider.Instance)}
}

// CreateInstance records the spec and returns a fake instance (fake-N with a
// fake IP). If CreateErr is set it fails without recording an instance. If
// OnCreate is set it is invoked with the spec before returning.
func (p *Provider) CreateInstance(_ context.Context, spec provider.InstanceSpec) (provider.Instance, error) {
	p.mu.Lock()
	if p.CreateErr != nil {
		err := p.CreateErr
		p.mu.Unlock()
		return provider.Instance{}, err
	}
	p.nextID++
	id := fmt.Sprintf("fake-%d", p.nextID)
	inst := provider.Instance{
		ID:       id,
		PublicIP: fmt.Sprintf("203.0.113.%d", p.nextID),
		Status:   "running",
	}
	p.creates = append(p.creates, spec)
	p.instances[id] = inst
	onCreate := p.OnCreate
	p.mu.Unlock()

	if onCreate != nil {
		onCreate(spec)
	}
	return inst, nil
}

// DeleteInstance records the id and is idempotent: deleting an unknown id (or
// the same id twice) returns nil.
func (p *Provider) DeleteInstance(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, id)
	delete(p.instances, id)
	return nil
}

// CreateImage records the name and returns a stub image.
func (p *Provider) CreateImage(_ context.Context, _ /*fromInstance*/, name string) (provider.Image, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.images = append(p.images, name)
	return provider.Image{ID: "fake-image-" + name, Name: name}, nil
}

// FindImage always reports the image as absent (no error).
func (p *Provider) FindImage(_ context.Context, _ string) (provider.Image, bool, error) {
	return provider.Image{}, false, nil
}

// EnsureFirewall records the rules and returns nil.
func (p *Provider) EnsureFirewall(_ context.Context, rules []provider.FirewallRule) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.firewalls = append(p.firewalls, rules)
	return nil
}

// CreateCount returns how many times CreateInstance was called (successfully).
func (p *Provider) CreateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.creates)
}

// DeleteCount returns how many times DeleteInstance was called (including
// repeats on the same id).
func (p *Provider) DeleteCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.deleted)
}

// LastUserData returns the UserData of the most recent CreateInstance spec, or
// "" if none.
func (p *Provider) LastUserData() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.creates) == 0 {
		return ""
	}
	return p.creates[len(p.creates)-1].UserData
}

// Deleted reports whether DeleteInstance was ever called with id.
func (p *Provider) Deleted(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.deleted {
		if d == id {
			return true
		}
	}
	return false
}

// Instances returns a snapshot of the currently-live instances (created and
// not yet deleted).
func (p *Provider) Instances() []provider.Instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.Instance, 0, len(p.instances))
	for _, inst := range p.instances {
		out = append(out, inst)
	}
	return out
}
