package config

import (
	"strings"
	"testing"
	"time"
)

// TestResolveEnvPresent verifies that when the required key is present in the
// environment, resolve returns runWizard=false, no error, and the key is in the
// resolved config.
func TestResolveEnvPresent(t *testing.T) {
	env := map[string]string{"ECU_API_KEY": "k_live_123"}

	cfg, runWizard, err := resolve(env, nil, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve returned error: %v", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true, want false when ECU_API_KEY is set")
	}
	if cfg.APIKey != "k_live_123" {
		t.Fatalf("cfg.APIKey = %q, want %q", cfg.APIKey, "k_live_123")
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

// TestResolvePrecedenceEnvOverFile verifies env > file for ECU_LISTEN.
func TestResolvePrecedenceEnvOverFile(t *testing.T) {
	env := map[string]string{
		"ECU_API_KEY": "k", // satisfy required so we exercise precedence, not fail-fast
		"ECU_LISTEN":  "0.0.0.0:9000",
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
	env := map[string]string{"ECU_API_KEY": "k"}
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
	env := map[string]string{"ECU_API_KEY": "k"}

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
		"ECU_API_KEY":      "k",
		"ECU_MAX_SESSIONS": "12",
		"ECU_IDLE_TIMEOUT": "30m",
		"ECU_MAX_LIFETIME": "2h",
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
	bad := map[string]string{"ECU_API_KEY": "k", "ECU_IDLE_TIMEOUT": "not-a-duration"}
	if _, _, err := resolve(bad, nil, false, requiredKeys); err == nil {
		t.Fatalf("resolve accepted an invalid ECU_IDLE_TIMEOUT, want error")
	}
}
