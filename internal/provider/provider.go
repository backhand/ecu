// Package provider defines the cloud-neutral seam that everything
// cloud-related in the control plane goes through. Per the brief, NO code
// outside this package tree may import a cloud SDK or reference a specific
// cloud's concepts; the core talks only to the Provider interface and the
// neutral value types declared here.
//
// Hetzner is the only implementation shipped initially. It lives in the
// subpackage internal/provider/hcloud, which confines every reference to the
// hcloud-go SDK. That subpackage imports THIS package (to satisfy the
// interface) and registers itself with the factory via an init() hook (see
// Register / New). The factory therefore stays in this package while keeping
// zero hcloud references here: the import edge runs hcloud -> provider only,
// so there is no import cycle. Callers that want the Hetzner implementation
// available add a blank import of internal/provider/hcloud (done in cmd/ecu)
// so its init() registration runs.
package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Firewall rule direction values. Exported so the control plane can express
// firewall rules without importing any cloud SDK.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// Firewall rule protocol values. Exported for the same cloud-neutrality
// reason as the direction constants.
const (
	ProtocolTCP  = "tcp"
	ProtocolUDP  = "udp"
	ProtocolICMP = "icmp"
	ProtocolESP  = "esp"
	ProtocolGRE  = "gre"
)

// Provider abstracts all cloud operations the control plane performs. The
// Hetzner implementation is the only one shipped initially; future providers
// (DigitalOcean, AWS EC2, GCE) slot in as new packages implementing this
// interface without touching the control-plane core.
type Provider interface {
	// CreateInstance provisions a new instance from spec and waits until it is
	// running, returning its id and public IP. On error the caller must assume
	// nothing was created that needs teardown only if the returned Instance id
	// is empty; if an id is returned alongside an error the caller should still
	// attempt teardown to avoid leaking a paid instance.
	CreateInstance(ctx context.Context, spec InstanceSpec) (Instance, error)

	// DeleteInstance destroys the instance with the given provider id. It MUST
	// be idempotent: deleting an unknown / already-deleted instance returns nil
	// so teardown can be retried safely (a leaked instance is a recurring
	// bill).
	DeleteInstance(ctx context.Context, id string) error

	// DeleteInstancesByLabel destroys every managed instance carrying the given
	// label key=value, returning the number destroyed. It is the orphan-cleanup
	// primitive used by C7 pre-baking on startup to reap a temporary bake
	// instance leaked by a crashed previous run (labeled e.g. ecu-bake=1), so a
	// restart mid-bake never leaks a paid instance. Like DeleteInstance it is
	// best-effort and idempotent: matching nothing returns (0, nil).
	DeleteInstancesByLabel(ctx context.Context, key, value string) (int, error)

	// CreateImage snapshots fromInstance into a named image (pre-baking, C7).
	CreateImage(ctx context.Context, fromInstance, name string) (Image, error)

	// FindImage looks up an image by name. found is false (with err=nil) when no
	// such image exists; that is not an error.
	FindImage(ctx context.Context, name string) (Image, bool, error)

	// EnsureFirewall makes the managed firewall match rules, creating it if
	// absent and applying it to all managed instances. Passing nil rules
	// synthesizes the safe default: block all inbound, allow all outbound.
	EnsureFirewall(ctx context.Context, rules []FirewallRule) error
}

// InstanceSpec describes an instance to create, in cloud-neutral terms.
type InstanceSpec struct {
	// Name is the instance name (also used by some providers as a hostname).
	Name string

	// Type is the provider instance type/size (e.g. "cpx21"). Empty means the
	// factory Config.DefaultType is used.
	Type string

	// Region is the provider region/location (e.g. "hel1"). Empty means the
	// factory Config.DefaultRegion is used.
	Region string

	// BaseImage is the image to boot from. It carries WHICHEVER image the
	// control plane chose: the base OS image for a cold boot (e.g.
	// "ubuntu-24.04") OR a pre-baked image reference once C7 supplies one. The
	// provider does not distinguish the two — it boots whatever image name/id
	// it is given.
	BaseImage string

	// UserData is the cloud-init #cloud-config passed to the instance at boot.
	UserData string

	// Labels are provider labels to attach (the Hetzner impl always also adds
	// ecu=managed so the managed firewall's label selector matches).
	Labels map[string]string

	// SSHKeyNames optionally names SSH keys (by provider key name) to install.
	// Usually empty: the agent dials out over the tunnel, so no inbound SSH is
	// required. Useful only for operator debugging.
	SSHKeyNames []string
}

// Instance is a created instance, in cloud-neutral terms.
type Instance struct {
	ID       string
	PublicIP string
	Status   string
}

// Image is a provider image/snapshot, in cloud-neutral terms.
type Image struct {
	ID   string
	Name string
}

// FirewallRule is a cloud-neutral firewall rule. Direction is DirectionIn /
// DirectionOut; Protocol is one of the Protocol* constants; Port is a single
// port or a range string (provider-specific; empty for protocols without
// ports). SourceCIDRs apply to inbound rules, DestinationCIDRs to outbound.
type FirewallRule struct {
	Direction        string
	Protocol         string
	Port             string
	SourceCIDRs      []string
	DestinationCIDRs []string
}

// Config is the factory input. The hcloud implementation reads Token, and uses
// DefaultType / DefaultRegion as fallbacks when an InstanceSpec leaves Type /
// Region blank. Labels are merged into every created instance's labels.
type Config struct {
	Token         string
	DefaultType   string
	DefaultRegion string
	Labels        map[string]string
}

// constructors holds the registered provider constructors keyed by lowercased
// provider name. Implementations register themselves from an init() so this
// package never imports any cloud SDK.
var constructors = map[string]func(Config) (Provider, error){}

// Register associates a provider name with its constructor. It is called from
// an implementation package's init() (e.g. internal/provider/hcloud registers
// "hetzner"). Names are matched case-insensitively in New.
func Register(name string, fn func(Config) (Provider, error)) {
	constructors[strings.ToLower(name)] = fn
}

// New builds the Provider selected by name. An empty name defaults to
// "hetzner". Matching is case-insensitive. It returns a clear error for an
// unknown provider, listing the registered ones. The implementation package
// must have been imported (so its init() ran) for its name to be known; the
// control plane arranges this with a blank import of the hcloud package.
func New(name string, cfg Config) (Provider, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = "hetzner"
	}
	fn, ok := constructors[key]
	if !ok {
		return nil, fmt.Errorf("provider: unknown provider %q (supported: %s)", name, supportedList())
	}
	return fn(cfg)
}

// supportedList returns the registered provider names in sorted order for
// deterministic error messages.
func supportedList() string {
	names := make([]string, 0, len(constructors))
	for n := range constructors {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "hetzner"
	}
	return strings.Join(names, ", ")
}
