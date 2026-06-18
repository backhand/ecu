package config

import (
	"strings"
	"testing"
	"time"
)

// TestTLSConfigDefaultsOff verifies the C10 TLS knobs default safely: ECU_TLS
// defaults to "off" (plain HTTP, the dev/Ingress-fronted default) and
// ECU_TLS_CACHE_DIR defaults to the home-relative autocert cache, with the
// leading ~ expanded. The full production (hetzner) required set —
// ECU_API_KEY + ECU_HCLOUD_TOKEN + ECU_INSTANCE_TYPE + ECU_REGION — is set so
// the path does not fail-fast; we are exercising defaults, not the required-key
// decision.
func TestTLSConfigDefaultsOff(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k", "ECU_HCLOUD_TOKEN": "hc_tok", "ECU_INSTANCE_TYPE": "cpx32", "ECU_REGION": "nbg1"}

	cfg, _, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.TLS != "off" {
		t.Fatalf("cfg.TLS = %q, want default %q", cfg.TLS, "off")
	}
	if cfg.TLS != defaultTLS {
		t.Fatalf("cfg.TLS = %q, want defaultTLS %q", cfg.TLS, defaultTLS)
	}
	// The cache dir default is home-relative; expandHome must have stripped the
	// leading ~ (an absolute path), and it must still end in the documented
	// suffix.
	if strings.HasPrefix(cfg.TLSCacheDir, "~") {
		t.Fatalf("cfg.TLSCacheDir = %q still has an unexpanded ~ prefix", cfg.TLSCacheDir)
	}
	if !strings.HasSuffix(cfg.TLSCacheDir, "/.local/share/ecu/tls") {
		t.Fatalf("cfg.TLSCacheDir = %q, want it to end in /.local/share/ecu/tls", cfg.TLSCacheDir)
	}
}

// TestTLSConfigEnvOverride verifies ECU_TLS=auto is carried through verbatim and
// that an explicit ECU_TLS_CACHE_DIR (env) overrides the default. Setting
// ECU_TLS=auto must NOT make resolve require a hostname or fail-fast.
func TestTLSConfigEnvOverride(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":       "k",
		"ECU_HCLOUD_TOKEN":  "hc_tok",
		"ECU_INSTANCE_TYPE": "cpx32",
		"ECU_REGION":        "nbg1",
		"ECU_TLS":           "auto",
		"ECU_TLS_CACHE_DIR": "/var/lib/ecu/tls",
	}

	cfg, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (ECU_TLS=auto must not add a required key)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false: ECU_TLS=auto must not force the wizard")
	}
	if cfg.TLS != "auto" {
		t.Fatalf("cfg.TLS = %q, want %q (carried through verbatim)", cfg.TLS, "auto")
	}
	if cfg.TLSCacheDir != "/var/lib/ecu/tls" {
		t.Fatalf("cfg.TLSCacheDir = %q, want the explicit env value", cfg.TLSCacheDir)
	}
}

// TestTLSConfigPrecedenceEnvOverFile verifies env > file for ECU_TLS.
func TestTLSConfigPrecedenceEnvOverFile(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":        "k",
		"ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
		"ECU_TLS":            "auto",
	}
	fileCfg := &Config{TLS: "off"}

	cfg, _, err := resolve(env, fileCfg, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.TLS != "auto" {
		t.Fatalf("cfg.TLS = %q, want env value %q (env must override file)", cfg.TLS, "auto")
	}
}

// TestResolveEnvPresent verifies that when the required keys are present in the
// environment, resolve returns runWizard=false, no error, and the key is in the
// resolved config. The default provider is hetzner and no dev tool-server is
// set, so production rules apply: ECU_HCLOUD_TOKEN, ECU_INSTANCE_TYPE, and
// ECU_REGION are required alongside ECU_API_KEY.
func TestResolveEnvPresent(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k_live_123", "ECU_HCLOUD_TOKEN": "hc_tok", "ECU_INSTANCE_TYPE": "cpx32", "ECU_REGION": "nbg1"}

	cfg, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false when required keys are set")
	}
	if cfg.APIKey != "k_live_123" {
		t.Fatalf("cfg.APIKey = %q, want %q", cfg.APIKey, "k_live_123")
	}
}

// TestResolveDevToolServerOnlyNeedsAPIKey verifies that the dev-toolserver path
// provisions nothing, so only ECU_API_KEY is required even on the (default)
// hetzner provider — no ECU_HCLOUD_TOKEN needed.
func TestResolveDevToolServerOnlyNeedsAPIKey(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":        "k",
		"ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
	}
	_, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (dev-toolserver mode must not require a provider token)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false: dev mode needs only ECU_API_KEY")
	}
}

// TestResolveProductionRequiresHCloudToken verifies the context-dependent
// requirement: production (no dev tool-server) on the hetzner provider requires
// ECU_HCLOUD_TOKEN, and its absence fails fast with no TTY, naming the key.
func TestResolveProductionRequiresHCloudToken(t *testing.T) {
	// Supply type + region so the ONLY missing key is the token; this isolates
	// the assertion that the token specifically is named.
	env := map[string]string{"ECU_API_KEY": "k", "ECU_INSTANCE_TYPE": "cpx32", "ECU_REGION": "nbg1"}

	_, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err == nil {
		t.Fatalf("resolve returned nil error, want fail-fast for missing ECU_HCLOUD_TOKEN")
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false on the fail-fast path")
	}
	if !strings.Contains(err.Error(), "ECU_HCLOUD_TOKEN") {
		t.Fatalf("error %q does not name the missing key ECU_HCLOUD_TOKEN", err.Error())
	}
}

// TestResolveAgentBinaryURLDefaults verifies ECU_AGENT_BINARY_URL is DEFAULTED
// (never required): when unset it resolves to the published release asset, so
// cloud-init always has a binary to fetch and provisioning never fails on a
// missing agent URL out of the box. It also must NOT appear in the required set
// — a production-hetzner deploy that omits it still resolves headless.
func TestResolveAgentBinaryURLDefaults(t *testing.T) {
	// Full production-hetzner required set, but deliberately NO
	// ECU_AGENT_BINARY_URL.
	env := map[string]string{
		"ECU_API_KEY": "k", "ECU_HCLOUD_TOKEN": "hc_tok",
		"ECU_INSTANCE_TYPE": "cpx32", "ECU_REGION": "nbg1",
	}
	cfg, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (agent_binary_url must be defaulted, not required)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false: a missing agent_binary_url must not force the wizard")
	}
	if cfg.AgentBinaryURL != defaultAgentBinaryURL {
		t.Fatalf("cfg.AgentBinaryURL = %q, want default %q", cfg.AgentBinaryURL, defaultAgentBinaryURL)
	}

	// Env overrides the default.
	env["ECU_AGENT_BINARY_URL"] = "https://example.com/ecu-custom"
	cfg, _, err = resolve(env, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.AgentBinaryURL != "https://example.com/ecu-custom" {
		t.Fatalf("cfg.AgentBinaryURL = %q, want the env override", cfg.AgentBinaryURL)
	}

	// File overrides the default (and env, absent, does not).
	cfg, _, err = resolve(
		map[string]string{
			"ECU_API_KEY": "k", "ECU_HCLOUD_TOKEN": "hc_tok",
			"ECU_INSTANCE_TYPE": "cpx32", "ECU_REGION": "nbg1",
		},
		&Config{AgentBinaryURL: "https://files/from-file"}, false, requiredKeys,
	)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.AgentBinaryURL != "https://files/from-file" {
		t.Fatalf("cfg.AgentBinaryURL = %q, want the file value (file beats default)", cfg.AgentBinaryURL)
	}
}

// TestResolveProductionRequiresInstanceTypeAndRegion verifies the new
// context-dependent requirement: production (no dev tool-server) on the hetzner
// provider requires ECU_INSTANCE_TYPE and ECU_REGION, and their absence
// fails fast with no TTY, naming BOTH missing keys (so a headless deploy gets a
// clear startup error instead of failing per-session at provision time).
func TestResolveProductionRequiresInstanceTypeAndRegion(t *testing.T) {
	// Token present so the only missing keys are type + region.
	env := map[string]string{"ECU_API_KEY": "k", "ECU_HCLOUD_TOKEN": "hc_tok"}

	_, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err == nil {
		t.Fatalf("resolve returned nil error, want fail-fast for missing ECU_INSTANCE_TYPE/ECU_REGION")
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false on the fail-fast path")
	}
	if !strings.Contains(err.Error(), "ECU_INSTANCE_TYPE") {
		t.Fatalf("error %q does not name the missing key ECU_INSTANCE_TYPE", err.Error())
	}
	if !strings.Contains(err.Error(), "ECU_REGION") {
		t.Fatalf("error %q does not name the missing key ECU_REGION", err.Error())
	}

	// With all four production keys present, it resolves headless.
	full := map[string]string{
		"ECU_API_KEY": "k", "ECU_HCLOUD_TOKEN": "hc_tok",
		"ECU_INSTANCE_TYPE": "cpx32", "ECU_REGION": "nbg1",
	}
	cfg, runWizard, err := resolve(full, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error with full production config: %v", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false when all required keys are set")
	}
	if cfg.InstanceType != "cpx32" || cfg.Region != "nbg1" {
		t.Fatalf("cfg.InstanceType=%q cfg.Region=%q, want cpx32/nbg1", cfg.InstanceType, cfg.Region)
	}
}

// TestResolveDevToolServerDoesNotRequireTypeOrRegion verifies that the
// dev-toolserver path (which provisions nothing) does NOT require
// ECU_INSTANCE_TYPE or ECU_REGION even on the default hetzner provider: only
// ECU_API_KEY is needed, and resolve succeeds headless with neither set.
func TestResolveDevToolServerDoesNotRequireTypeOrRegion(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":        "k",
		"ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
		// deliberately no ECU_INSTANCE_TYPE / ECU_REGION
	}
	cfg, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (dev-toolserver mode must not require type/region)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false: dev mode needs only ECU_API_KEY")
	}
	if cfg.InstanceType != "" || cfg.Region != "" {
		t.Fatalf("expected type/region unset in dev mode, got %q/%q", cfg.InstanceType, cfg.Region)
	}
}

// TestResolveNonHetznerDoesNotRequireTypeOrRegion verifies the type/region
// requirement is specific to the hetzner provider: a non-hetzner provider
// resolves headless without them (it has no hetzner-shaped ServerType/location
// requirement).
func TestResolveNonHetznerDoesNotRequireTypeOrRegion(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k", "ECU_PROVIDER": "someother"}
	_, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (non-hetzner provider must not require type/region)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false")
	}
}

// TestResolveNonHetznerProviderNoTokenRequired verifies that a non-hetzner
// provider does not require ECU_HCLOUD_TOKEN (the token requirement is specific
// to the hetzner provider).
func TestResolveNonHetznerProviderNoTokenRequired(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k", "ECU_PROVIDER": "someother"}
	_, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (non-hetzner provider must not require ECU_HCLOUD_TOKEN)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false")
	}
}

// TestResolveLocalProviderOnlyNeedsAPIKey verifies the local provider (which
// runs containers on-box and provisions no cloud instance) requires only
// ECU_API_KEY — no Hetzner token / type / region — and carries the provider
// name through. ECU_CONTAINER_IMAGE is supplied because the local provider runs
// it, but it is defaulted, so its presence is incidental here.
func TestResolveLocalProviderOnlyNeedsAPIKey(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":         "k",
		"ECU_PROVIDER":        "local",
		"ECU_CONTAINER_IMAGE": "ecu-image:dev",
	}
	cfg, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v (local provider must need only ECU_API_KEY)", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false: local provider needs only ECU_API_KEY")
	}
	if cfg.Provider != "local" {
		t.Fatalf("cfg.Provider = %q, want %q", cfg.Provider, "local")
	}
}

// TestResolveMissingWithTTY verifies that when the required key is missing but
// a TTY is present, resolve signals that the wizard should run, without error.
func TestResolveMissingWithTTY(t *testing.T) {
	env := map[string]string{} // no ECU_API_KEY

	cfg, runWizard, err := resolve(env, nil, true /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if !runWizard {
		t.Fatalf("runWizard = false, want true when required key missing and TTY present")
	}
	if cfg == nil {
		t.Fatalf("cfg is nil; resolve should return the merged config to fill in via the wizard")
	}
}

// TestResolveMissingNoTTY verifies the fail-fast path: required key missing and
// no TTY yields an error whose text names the missing key.
func TestResolveMissingNoTTY(t *testing.T) {
	env := map[string]string{} // no ECU_API_KEY

	_, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err == nil {
		t.Fatalf("resolve returned nil error, want fail-fast error")
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false on the fail-fast path")
	}
	if !strings.Contains(err.Error(), "ECU_API_KEY") {
		t.Fatalf("error %q does not name the missing key ECU_API_KEY", err.Error())
	}
}

// TestResolvePrecedenceEnvOverFile verifies env > file for ECU_LISTEN. The dev
// tool-server is set so only ECU_API_KEY is required and we exercise
// precedence, not the production token requirement.
func TestResolvePrecedenceEnvOverFile(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":        "k", // satisfy required so we exercise precedence, not fail-fast
		"ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
		"ECU_LISTEN":         "0.0.0.0:9000",
	}
	fileCfg := &Config{Listen: "127.0.0.1:7000"}

	cfg, _, err := resolve(env, fileCfg, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9000" {
		t.Fatalf("cfg.Listen = %q, want env value %q (env must override file)", cfg.Listen, "0.0.0.0:9000")
	}
}

// TestResolvePrecedenceFileOverDefault verifies file > default for ECU_LISTEN
// when env does not set it.
func TestResolvePrecedenceFileOverDefault(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000"}
	fileCfg := &Config{Listen: "127.0.0.1:7000"}

	cfg, _, err := resolve(env, fileCfg, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7000" {
		t.Fatalf("cfg.Listen = %q, want file value %q (file must override default)", cfg.Listen, "127.0.0.1:7000")
	}
}

// TestResolveDefaultWhenUnset verifies the built-in default for ECU_LISTEN is
// used when neither env nor file supplies it.
func TestResolveDefaultWhenUnset(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000"}

	cfg, _, err := resolve(env, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.Listen != defaultListen {
		t.Fatalf("cfg.Listen = %q, want default %q", cfg.Listen, defaultListen)
	}
}

// TestResolveParsesTypedSettings verifies ints and durations parse from env and
// that an invalid set value is reported as an error.
func TestResolveParsesTypedSettings(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY":        "k",
		"ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
		"ECU_MAX_SESSIONS":   "12",
		"ECU_IDLE_TIMEOUT":   "30m",
		"ECU_MAX_LIFETIME":   "2h",
	}
	cfg, _, err := resolve(env, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if cfg.MaxSessions != 12 {
		t.Fatalf("cfg.MaxSessions = %d, want 12", cfg.MaxSessions)
	}
	if cfg.IdleTimeout != 30*time.Minute {
		t.Fatalf("cfg.IdleTimeout = %v, want 30m", cfg.IdleTimeout)
	}
	if cfg.MaxLifetime != 2*time.Hour {
		t.Fatalf("cfg.MaxLifetime = %v, want 2h", cfg.MaxLifetime)
	}

	// A set-but-invalid duration must error.
	bad := map[string]string{"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000", "ECU_IDLE_TIMEOUT": "not-a-duration"}
	if _, _, err := resolve(bad, nil, false, requiredKeys); err == nil {
		t.Fatalf("resolve accepted an invalid ECU_IDLE_TIMEOUT, want error")
	}
}

// TestResolvePreBakeSettings exercises the C7 settings and, crucially, that
// ECU_IMAGE (the pre-baked SNAPSHOT NAME) and ECU_CONTAINER_IMAGE (the container
// image ref) are DISTINCT: ECU_IMAGE has no default (empty disables pre-baking),
// while ECU_CONTAINER_IMAGE defaults to defaultContainerImage. ECU_BAKE_TIMEOUT
// parses as a duration and defaults to defaultBakeTimeout.
func TestResolvePreBakeSettings(t *testing.T) {
	base := map[string]string{"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000"}

	// Defaults: ECU_IMAGE empty (no pre-baking), container image + bake timeout
	// take their defaults.
	cfg, _, err := resolve(base, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Image != "" {
		t.Fatalf("cfg.Image = %q, want empty by default (ECU_IMAGE controls pre-baking)", cfg.Image)
	}
	if cfg.ContainerImage != defaultContainerImage {
		t.Fatalf("cfg.ContainerImage = %q, want default %q", cfg.ContainerImage, defaultContainerImage)
	}
	if cfg.BakeTimeout != defaultBakeTimeout {
		t.Fatalf("cfg.BakeTimeout = %v, want default %v", cfg.BakeTimeout, defaultBakeTimeout)
	}

	// Env values: the snapshot name and container ref are independent values, and
	// the bake timeout parses.
	env := map[string]string{
		"ECU_API_KEY":         "k",
		"ECU_DEV_TOOLSERVER":  "http://127.0.0.1:8000",
		"ECU_IMAGE":           "ecu-prebaked",
		"ECU_CONTAINER_IMAGE": "ghcr.io/backhand/ecu-image:pinned",
		"ECU_BAKE_TIMEOUT":    "30m",
	}
	cfg, _, err = resolve(env, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Image != "ecu-prebaked" {
		t.Fatalf("cfg.Image (snapshot name) = %q, want ecu-prebaked", cfg.Image)
	}
	if cfg.ContainerImage != "ghcr.io/backhand/ecu-image:pinned" {
		t.Fatalf("cfg.ContainerImage = %q, want the pinned ref", cfg.ContainerImage)
	}
	if cfg.BakeTimeout != 30*time.Minute {
		t.Fatalf("cfg.BakeTimeout = %v, want 30m", cfg.BakeTimeout)
	}

	// Precedence: ECU_CONTAINER_IMAGE env beats file beats default.
	cfg, _, err = resolve(
		map[string]string{"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000"},
		&Config{ContainerImage: "ghcr.io/from-file:tag"}, false, requiredKeys,
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.ContainerImage != "ghcr.io/from-file:tag" {
		t.Fatalf("cfg.ContainerImage = %q, want file value (file beats default)", cfg.ContainerImage)
	}
}

// TestResolvePersistenceSettings exercises the C8 persistence settings:
// ECU_MAX_PERSISTENT_SESSIONS (int) and the two durations
// ECU_PERSISTENT_MAX_LIFETIME / ECU_PERSISTENT_MAX_AGE — defaults when unset,
// parsing from env, env>file precedence, and an invalid duration erroring.
func TestResolvePersistenceSettings(t *testing.T) {
	base := map[string]string{"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000"}

	// Defaults when unset.
	cfg, _, err := resolve(base, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.MaxPersistentSessions != defaultMaxPersistentSessions {
		t.Fatalf("cfg.MaxPersistentSessions = %d, want default %d", cfg.MaxPersistentSessions, defaultMaxPersistentSessions)
	}
	if cfg.PersistentMaxLifetime != defaultPersistentMaxLifetime {
		t.Fatalf("cfg.PersistentMaxLifetime = %v, want default %v", cfg.PersistentMaxLifetime, defaultPersistentMaxLifetime)
	}
	if cfg.PersistentMaxAge != defaultPersistentMaxAge {
		t.Fatalf("cfg.PersistentMaxAge = %v, want default %v", cfg.PersistentMaxAge, defaultPersistentMaxAge)
	}

	// Parse from env.
	env := map[string]string{
		"ECU_API_KEY":                 "k",
		"ECU_DEV_TOOLSERVER":          "http://127.0.0.1:8000",
		"ECU_MAX_PERSISTENT_SESSIONS": "5",
		"ECU_PERSISTENT_MAX_LIFETIME": "12h",
		"ECU_PERSISTENT_MAX_AGE":      "72h",
	}
	cfg, _, err = resolve(env, nil, false, requiredKeys)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.MaxPersistentSessions != 5 {
		t.Fatalf("cfg.MaxPersistentSessions = %d, want 5", cfg.MaxPersistentSessions)
	}
	if cfg.PersistentMaxLifetime != 12*time.Hour {
		t.Fatalf("cfg.PersistentMaxLifetime = %v, want 12h", cfg.PersistentMaxLifetime)
	}
	if cfg.PersistentMaxAge != 72*time.Hour {
		t.Fatalf("cfg.PersistentMaxAge = %v, want 72h", cfg.PersistentMaxAge)
	}

	// env > file precedence for the durations and the int.
	cfg, _, err = resolve(
		map[string]string{
			"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
			"ECU_PERSISTENT_MAX_LIFETIME": "6h", "ECU_MAX_PERSISTENT_SESSIONS": "9",
		},
		&Config{PersistentMaxLifetime: 99 * time.Hour, PersistentMaxAge: 48 * time.Hour, MaxPersistentSessions: 1},
		false, requiredKeys,
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.PersistentMaxLifetime != 6*time.Hour {
		t.Fatalf("cfg.PersistentMaxLifetime = %v, want env 6h (env beats file)", cfg.PersistentMaxLifetime)
	}
	if cfg.MaxPersistentSessions != 9 {
		t.Fatalf("cfg.MaxPersistentSessions = %d, want env 9 (env beats file)", cfg.MaxPersistentSessions)
	}
	// PersistentMaxAge only in file -> file value used (file beats default).
	if cfg.PersistentMaxAge != 48*time.Hour {
		t.Fatalf("cfg.PersistentMaxAge = %v, want file 48h (file beats default)", cfg.PersistentMaxAge)
	}

	// Invalid duration errors.
	bad := map[string]string{
		"ECU_API_KEY": "k", "ECU_DEV_TOOLSERVER": "http://127.0.0.1:8000",
		"ECU_PERSISTENT_MAX_AGE": "not-a-duration",
	}
	if _, _, err := resolve(bad, nil, false, requiredKeys); err == nil {
		t.Fatalf("resolve accepted an invalid ECU_PERSISTENT_MAX_AGE, want error")
	}
}
