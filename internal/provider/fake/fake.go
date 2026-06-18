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
	// instanceLabels maps a live instance id to the labels its create spec
	// carried, so DeleteInstancesByLabel can match the way a real provider's
	// label selector would.
	instanceLabels map[string]map[string]string
	// deleted records DeleteInstance ids in call order (including repeats).
	deleted []string

	// images records each CreateImage call (its source instance + name) in
	// order; firewalls records EnsureFirewall calls.
	images    []ImageCall
	firewalls [][]provider.FirewallRule

	// CreateErr, when non-nil, makes CreateInstance fail with it (and record
	// nothing as created). Set before the call.
	CreateErr error

	// CreateImageErr, when non-nil, makes CreateImage fail with it (and record
	// nothing). Lets C7 bake tests exercise the snapshot-failure teardown path.
	CreateImageErr error

	// FindImageResult, when set, is what FindImage returns. The default (nil)
	// keeps the historical behavior of always reporting the image absent. C7
	// startup tests set it to model a pre-existing snapshot (found=true).
	FindImageResult *FindImageResult

	// OnCreate, when set, is invoked (synchronously, inside CreateInstance,
	// after the instance is recorded and BEFORE CreateInstance returns) with
	// the spec. A test typically uses it to, in a goroutine, launch the real
	// agent against the test control plane using the token parsed out of
	// spec.UserData — driving the session to ready. C7 bake tests also use it to
	// fire the bake-completion callback once the bake instance is created.
	OnCreate func(provider.InstanceSpec)
}

// ImageCall records a single CreateImage invocation.
type ImageCall struct {
	FromInstance string
	Name         string
}

// FindImageResult is the canned outcome FindImage returns when
// Provider.FindImageResult is set.
type FindImageResult struct {
	Image provider.Image
	Found bool
	Err   error
}

// New returns an empty fake provider.
func New() *Provider {
	return &Provider{
		instances:      make(map[string]provider.Instance),
		instanceLabels: make(map[string]map[string]string),
	}
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
	if len(spec.Labels) > 0 {
		labels := make(map[string]string, len(spec.Labels))
		for k, v := range spec.Labels {
			labels[k] = v
		}
		p.instanceLabels[id] = labels
	}
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
	delete(p.instanceLabels, id)
	return nil
}

// DeleteInstancesByLabel deletes every LIVE instance whose create spec carried
// the label key=value, returning the count destroyed. It records each deletion
// like DeleteInstance (so DeleteCount/Deleted observe them) and is idempotent:
// matching nothing returns (0, nil). This mirrors a real provider's
// label-selector delete, letting C7 startup orphan-cleanup tests assert that a
// leaked ecu-bake instance is reaped.
func (p *Provider) DeleteInstancesByLabel(_ context.Context, key, value string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var match []string
	for id, labels := range p.instanceLabels {
		if labels[key] == value {
			match = append(match, id)
		}
	}
	for _, id := range match {
		p.deleted = append(p.deleted, id)
		delete(p.instances, id)
		delete(p.instanceLabels, id)
	}
	return len(match), nil
}

// CreateImage records the call (source instance + name) and returns a stub
// image, unless CreateImageErr is set (then it fails and records nothing).
func (p *Provider) CreateImage(_ context.Context, fromInstance, name string) (provider.Image, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.CreateImageErr != nil {
		return provider.Image{}, p.CreateImageErr
	}
	p.images = append(p.images, ImageCall{FromInstance: fromInstance, Name: name})
	return provider.Image{ID: "fake-image-" + name, Name: name}, nil
}

// FindImage returns FindImageResult if set, else reports the image absent (the
// historical default). The name is ignored: tests set the canned result they
// want for the single image under test.
func (p *Provider) FindImage(_ context.Context, _ string) (provider.Image, bool, error) {
	p.mu.Lock()
	r := p.FindImageResult
	p.mu.Unlock()
	if r != nil {
		return r.Image, r.Found, r.Err
	}
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

// Creates returns a snapshot of every CreateInstance spec in call order. Tests
// use it to assert per-create details such as the BaseImage (e.g. that a session
// provisioned after a bake boots from the snapshot).
func (p *Provider) Creates() []provider.InstanceSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]provider.InstanceSpec, len(p.creates))
	copy(out, p.creates)
	return out
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

// Images returns a snapshot of the CreateImage calls in order.
func (p *Provider) Images() []ImageCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ImageCall, len(p.images))
	copy(out, p.images)
	return out
}

// ImageCount returns how many times CreateImage was called (successfully).
func (p *Provider) ImageCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.images)
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
