// Package hcloud is the Hetzner Cloud implementation of provider.Provider. It
// is the ONLY package in the repo that imports the hcloud-go SDK; the SDK is
// imported under the alias hcloudapi so this package's own name (hcloud) does
// not collide with it. Per the brief, no code outside this package tree may
// reference Hetzner concepts — the control plane talks only to
// provider.Provider.
//
// The package registers itself with the provider factory from init(), so a
// blank import of this package (done in cmd/ecu) makes provider.New("hetzner",
// ...) work without the factory importing any SDK (which would be a cycle).
package hcloud

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/backhand/ecu/internal/provider"
	hcloudapi "github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// managedLabelKey/Value tag every instance and the managed firewall so the
// firewall's label selector auto-applies to all managed servers.
const (
	managedLabelKey   = "ecu"
	managedLabelValue = "managed"
	// managedFirewallName is the single firewall ECU manages.
	managedFirewallName = "ecu-managed"
	// imageLabelKey labels pre-baked images so FindImage can select them.
	imageLabelKey = "ecu-image"
)

func init() {
	provider.Register("hetzner", func(cfg provider.Config) (provider.Provider, error) {
		return New(cfg)
	})
}

// Provider implements provider.Provider against the Hetzner Cloud API.
type Provider struct {
	client *hcloudapi.Client
	cfg    provider.Config
}

// New builds a Hetzner provider from cfg, authenticating with cfg.Token.
func New(cfg provider.Config) (*Provider, error) {
	return newWithOptions(cfg, hcloudapi.WithToken(cfg.Token))
}

// newWithOptions builds a provider with the given base options plus any extra
// ClientOptions. Tests use it to inject WithEndpoint(testServer.URL) and
// WithPollOpts(ConstantBackoff(0)) so action polling is instant and no live
// call is made. cfg.Token may be empty in tests (the endpoint override is what
// matters).
func newWithOptions(cfg provider.Config, opts ...hcloudapi.ClientOption) (*Provider, error) {
	client := hcloudapi.NewClient(opts...)
	return &Provider{client: client, cfg: cfg}, nil
}

// CreateInstance provisions a server, waits for its actions to complete, then
// re-fetches it to populate the public IP (which may be empty in the create
// response). The instance always carries the ecu=managed label so the managed
// firewall applies to it.
func (p *Provider) CreateInstance(ctx context.Context, spec provider.InstanceSpec) (provider.Instance, error) {
	serverType := spec.Type
	if serverType == "" {
		serverType = p.cfg.DefaultType
	}
	region := spec.Region
	if region == "" {
		region = p.cfg.DefaultRegion
	}

	opts := hcloudapi.ServerCreateOpts{
		Name:       spec.Name,
		ServerType: &hcloudapi.ServerType{Name: serverType},
		Image:      imageRef(spec.BaseImage),
		UserData:   spec.UserData,
		Labels:     p.mergeLabels(spec.Labels),
	}
	if region != "" {
		opts.Location = &hcloudapi.Location{Name: region}
	}
	for _, name := range spec.SSHKeyNames {
		opts.SSHKeys = append(opts.SSHKeys, &hcloudapi.SSHKey{Name: name})
	}

	res, _, err := p.client.Server.Create(ctx, opts)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("hcloud: creating server: %w", err)
	}

	// Wait for the create action and any follow-up actions to finish (guarding
	// nils). WaitFor returns immediately for already-succeeded actions.
	actions := make([]*hcloudapi.Action, 0, 1+len(res.NextActions))
	if res.Action != nil {
		actions = append(actions, res.Action)
	}
	actions = append(actions, res.NextActions...)
	if len(actions) > 0 {
		if err := p.client.Action.WaitFor(ctx, actions...); err != nil {
			// The server exists at this point; return its id alongside the error
			// so the caller can tear it down rather than leak it.
			id := ""
			if res.Server != nil {
				id = strconv.FormatInt(res.Server.ID, 10)
			}
			return provider.Instance{ID: id}, fmt.Errorf("hcloud: waiting for server actions: %w", err)
		}
	}

	if res.Server == nil {
		return provider.Instance{}, fmt.Errorf("hcloud: create returned no server")
	}
	id := res.Server.ID

	// Re-fetch to populate the public IP, which is frequently empty in the
	// create response until the actions complete.
	ip := publicIPv4(res.Server)
	status := string(res.Server.Status)
	if fresh, _, err := p.client.Server.GetByID(ctx, id); err == nil && fresh != nil {
		if got := publicIPv4(fresh); got != "" {
			ip = got
		}
		status = string(fresh.Status)
	}

	return provider.Instance{
		ID:       strconv.FormatInt(id, 10),
		PublicIP: ip,
		Status:   status,
	}, nil
}

// DeleteInstance destroys the server with the given id. It is idempotent: a
// non-numeric id (never a real instance) and a not_found response both return
// nil so teardown can be retried safely and never wedges. Other errors
// propagate.
func (p *Provider) DeleteInstance(ctx context.Context, id string) error {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		// A bad id can never correspond to a real instance; treat as nothing to
		// delete rather than wedging teardown.
		return nil
	}
	_, err = p.client.Server.Delete(ctx, &hcloudapi.Server{ID: n})
	if err != nil {
		if hcloudapi.IsError(err, hcloudapi.ErrorCodeNotFound) {
			return nil // already gone — idempotent
		}
		return fmt.Errorf("hcloud: deleting server %s: %w", id, err)
	}
	return nil
}

// DeleteInstancesByLabel lists managed servers matching key=value and deletes
// each, returning how many were destroyed. Used by C7 startup orphan cleanup to
// reap a leaked bake instance. It is best-effort and idempotent: it tolerates a
// server vanishing between the list and the delete (not_found -> skip) and
// returns the count actually destroyed. A list error is reported; per-server
// delete errors are aggregated so one bad server does not hide the rest.
func (p *Provider) DeleteInstancesByLabel(ctx context.Context, key, value string) (int, error) {
	servers, err := p.client.Server.AllWithOpts(ctx, hcloudapi.ServerListOpts{
		ListOpts: hcloudapi.ListOpts{LabelSelector: key + "=" + value},
	})
	if err != nil {
		return 0, fmt.Errorf("hcloud: listing servers by label %s=%s: %w", key, value, err)
	}
	deleted := 0
	var firstErr error
	for _, srv := range servers {
		if _, err := p.client.Server.Delete(ctx, srv); err != nil {
			if hcloudapi.IsError(err, hcloudapi.ErrorCodeNotFound) {
				continue // already gone between list and delete
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("hcloud: deleting server %d: %w", srv.ID, err)
			}
			continue
		}
		deleted++
	}
	return deleted, firstErr
}

// CreateImage snapshots fromInstance into a named image (C7 pre-baking). The
// name is carried as the image Description and an ecu-image=<name> label so
// FindImage can select it.
func (p *Provider) CreateImage(ctx context.Context, fromInstance, name string) (provider.Image, error) {
	n, err := strconv.ParseInt(fromInstance, 10, 64)
	if err != nil {
		return provider.Image{}, fmt.Errorf("hcloud: invalid instance id %q: %w", fromInstance, err)
	}
	desc := name
	opts := &hcloudapi.ServerCreateImageOpts{
		Type:        hcloudapi.ImageTypeSnapshot,
		Description: &desc,
		Labels: map[string]string{
			managedLabelKey: managedLabelValue,
			imageLabelKey:   name,
		},
	}
	res, _, err := p.client.Server.CreateImage(ctx, &hcloudapi.Server{ID: n}, opts)
	if err != nil {
		return provider.Image{}, fmt.Errorf("hcloud: creating image from server %s: %w", fromInstance, err)
	}
	if res.Action != nil {
		if err := p.client.Action.WaitFor(ctx, res.Action); err != nil {
			return provider.Image{}, fmt.Errorf("hcloud: waiting for image action: %w", err)
		}
	}
	if res.Image == nil {
		return provider.Image{}, fmt.Errorf("hcloud: create image returned no image")
	}
	return provider.Image{ID: strconv.FormatInt(res.Image.ID, 10), Name: name}, nil
}

// FindImage looks up a pre-baked image by its ecu-image label. found is false
// (err=nil) when no such image exists.
func (p *Provider) FindImage(ctx context.Context, name string) (provider.Image, bool, error) {
	images, err := p.client.Image.AllWithOpts(ctx, hcloudapi.ImageListOpts{
		ListOpts: hcloudapi.ListOpts{LabelSelector: imageLabelKey + "=" + name},
	})
	if err != nil {
		return provider.Image{}, false, fmt.Errorf("hcloud: listing images: %w", err)
	}
	if len(images) == 0 {
		return provider.Image{}, false, nil
	}
	img := images[0]
	return provider.Image{ID: strconv.FormatInt(img.ID, 10), Name: name}, true, nil
}

// EnsureFirewall makes the managed firewall match rules. Passing nil rules
// synthesizes the safe default: NO inbound allow rules (Hetzner's default-deny
// inbound then blocks all inbound) plus a single allow-all outbound rule for
// tcp+udp to 0.0.0.0/0 and ::/0 (so the agent's WSS dial-out works). If the
// firewall exists its rules are replaced; otherwise it is created with a
// label-selector resource so it auto-applies to every ecu=managed server.
func (p *Provider) EnsureFirewall(ctx context.Context, rules []provider.FirewallRule) error {
	apiRules, err := toAPIRules(rules)
	if err != nil {
		return err
	}

	existing, err := p.client.Firewall.AllWithOpts(ctx, hcloudapi.FirewallListOpts{Name: managedFirewallName})
	if err != nil {
		return fmt.Errorf("hcloud: listing firewalls: %w", err)
	}

	if len(existing) > 0 {
		fw := existing[0]
		actions, _, err := p.client.Firewall.SetRules(ctx, fw, hcloudapi.FirewallSetRulesOpts{Rules: apiRules})
		if err != nil {
			return fmt.Errorf("hcloud: setting firewall rules: %w", err)
		}
		if err := p.client.Action.WaitFor(ctx, actions...); err != nil {
			return fmt.Errorf("hcloud: waiting for set-rules actions: %w", err)
		}
		return nil
	}

	res, _, err := p.client.Firewall.Create(ctx, hcloudapi.FirewallCreateOpts{
		Name:   managedFirewallName,
		Labels: map[string]string{managedLabelKey: managedLabelValue},
		Rules:  apiRules,
		ApplyTo: []hcloudapi.FirewallResource{{
			Type: hcloudapi.FirewallResourceTypeLabelSelector,
			LabelSelector: &hcloudapi.FirewallResourceLabelSelector{
				Selector: managedLabelKey + "=" + managedLabelValue,
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("hcloud: creating firewall: %w", err)
	}
	if err := p.client.Action.WaitFor(ctx, res.Actions...); err != nil {
		return fmt.Errorf("hcloud: waiting for firewall-create actions: %w", err)
	}
	return nil
}

// mergeLabels merges the factory labels and the spec labels, always including
// ecu=managed so the managed firewall's label selector matches.
func (p *Provider) mergeLabels(specLabels map[string]string) map[string]string {
	out := make(map[string]string, len(p.cfg.Labels)+len(specLabels)+1)
	for k, v := range p.cfg.Labels {
		out[k] = v
	}
	for k, v := range specLabels {
		out[k] = v
	}
	out[managedLabelKey] = managedLabelValue
	return out
}

// imageRef builds the hcloud Image reference to boot from for a given
// InstanceSpec.BaseImage. Hetzner distinguishes a NAMED OS image (e.g.
// "ubuntu-24.04") from a SNAPSHOT, which is referenced by its numeric id — a
// snapshot has no name. C7 (pre-baking) supplies a snapshot's id as BaseImage,
// so an all-digit value is treated as an image ID and anything else as a name.
// This is the footgun the brief calls out: booting a snapshot via {Name: ...}
// silently fails because snapshots are nameless.
func imageRef(baseImage string) *hcloudapi.Image {
	if n, err := strconv.ParseInt(baseImage, 10, 64); err == nil {
		return &hcloudapi.Image{ID: n}
	}
	return &hcloudapi.Image{Name: baseImage}
}

// publicIPv4 returns the server's public IPv4 as a string, or "" if unset.
func publicIPv4(s *hcloudapi.Server) string {
	if s == nil {
		return ""
	}
	ip := s.PublicNet.IPv4.IP
	if ip == nil || ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

// toAPIRules converts cloud-neutral rules to hcloud rules. nil/empty rules
// yield the safe default (block inbound, allow outbound). CIDRs are parsed to
// net.IPNet; a single port maps to a *string Port (only when non-empty).
func toAPIRules(rules []provider.FirewallRule) ([]hcloudapi.FirewallRule, error) {
	if len(rules) == 0 {
		return defaultOutboundRules()
	}
	out := make([]hcloudapi.FirewallRule, 0, len(rules))
	for _, r := range rules {
		ar := hcloudapi.FirewallRule{
			Direction: hcloudapi.FirewallRuleDirection(r.Direction),
			Protocol:  hcloudapi.FirewallRuleProtocol(r.Protocol),
		}
		if r.Port != "" {
			port := r.Port
			ar.Port = &port
		}
		src, err := parseCIDRs(r.SourceCIDRs)
		if err != nil {
			return nil, err
		}
		dst, err := parseCIDRs(r.DestinationCIDRs)
		if err != nil {
			return nil, err
		}
		ar.SourceIPs = src
		ar.DestinationIPs = dst
		out = append(out, ar)
	}
	return out, nil
}

// defaultOutboundRules is the "block inbound, allow outbound" default: no
// inbound allow rules (default-deny inbound blocks everything), one outbound
// allow-all for tcp and one for udp, each to 0.0.0.0/0 and ::/0.
func defaultOutboundRules() ([]hcloudapi.FirewallRule, error) {
	all, err := parseCIDRs([]string{"0.0.0.0/0", "::/0"})
	if err != nil {
		return nil, err
	}
	return []hcloudapi.FirewallRule{
		{
			Direction:      hcloudapi.FirewallRuleDirectionOut,
			Protocol:       hcloudapi.FirewallRuleProtocolTCP,
			DestinationIPs: all,
		},
		{
			Direction:      hcloudapi.FirewallRuleDirectionOut,
			Protocol:       hcloudapi.FirewallRuleProtocolUDP,
			DestinationIPs: all,
		},
	}, nil
}

// parseCIDRs parses each CIDR string into a net.IPNet (the masked network, as
// Hetzner expects). An empty slice yields nil.
func parseCIDRs(cidrs []string) ([]net.IPNet, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	out := make([]net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("hcloud: invalid CIDR %q: %w", c, err)
		}
		out = append(out, *ipnet)
	}
	return out, nil
}
