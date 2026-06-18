package config

import (
	"strings"
	"testing"
	"time"
)

// TestResolveEnvPresent verifies that when the required keys are present in the
// environment, resolve returns runWizard=false, no error, and the key is in the
// resolved config. The default provider is hetzner and no dev tool-server is
// set, so production rules apply: ECU_HCLOUD_TOKEN is required alongside
// ECU_API_KEY.
func TestResolveEnvPresent(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k_live_123", "ECU_HCLOUD_TOKEN": "hc_tok"}

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
	env := map[string]string{"ECU_API_KEY": "k"} // hetzner default, no dev seam, no token

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
