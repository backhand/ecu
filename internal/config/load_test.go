package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStdinIsTTYNullDevice is a regression test for the fail-fast rule: a
// container or k3s pod with no TTY allocated gets /dev/null as stdin, which is a
// character device. A naive (mode & ModeCharDevice) check misclassifies it as a
// terminal and sends the no-TTY deploy into the interactive wizard, blocking on
// stdin — the exact hang fail-fast exists to prevent. stdinIsTTY must report
// false for the null device.
func TestStdinIsTTYNullDevice(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer null.Close()

	saved := os.Stdin
	os.Stdin = null
	defer func() { os.Stdin = saved }()

	if stdinIsTTY() {
		t.Fatalf("stdinIsTTY() = true for %s, want false (no-TTY deploys must fail fast, not enter the wizard)", os.DevNull)
	}
}

// TestStdinIsTTYPipe verifies that a pipe (non-character-device) stdin is also
// reported as non-interactive, so a piped invocation fails fast rather than
// prompting.
func TestStdinIsTTYPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	saved := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = saved }()

	if stdinIsTTY() {
		t.Fatalf("stdinIsTTY() = true for a pipe, want false")
	}
}

// TestFileConfigRoundTrip verifies that writing a Config to disk and reading it
// back preserves the fields, including durations stored as strings.
func TestFileConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")

	original := &Config{
		Hostname:              "ecu.example.com",
		Listen:                "0.0.0.0:8443",
		APIKey:                "k_admin",
		Provider:              "hetzner",
		MaxSessions:           5,
		IdleTimeout:           45 * time.Minute,
		MaxLifetime:           4 * time.Hour,
		MaxPersistentSessions: 2,
		DevToolServer:         "http://127.0.0.1:8000",
	}

	if err := writeFileConfig(path, original); err != nil {
		t.Fatalf("writeFileConfig: %v", err)
	}

	reloaded, err := readFileConfig(path)
	if err != nil {
		t.Fatalf("readFileConfig: %v", err)
	}

	if reloaded.Hostname != original.Hostname ||
		reloaded.Listen != original.Listen ||
		reloaded.APIKey != original.APIKey ||
		reloaded.Provider != original.Provider ||
		reloaded.MaxSessions != original.MaxSessions ||
		reloaded.MaxPersistentSessions != original.MaxPersistentSessions ||
		reloaded.DevToolServer != original.DevToolServer {
		t.Fatalf("string/int fields did not round-trip:\n got  %+v\n want %+v", reloaded, original)
	}
	if reloaded.IdleTimeout != original.IdleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v", reloaded.IdleTimeout, original.IdleTimeout)
	}
	if reloaded.MaxLifetime != original.MaxLifetime {
		t.Fatalf("MaxLifetime = %v, want %v", reloaded.MaxLifetime, original.MaxLifetime)
	}
}

// TestReadFileConfigMissing verifies that a missing config file is tolerated:
// it returns an empty Config with no error.
func TestReadFileConfigMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	cfg, err := readFileConfig(path)
	if err != nil {
		t.Fatalf("readFileConfig on missing file returned error: %v", err)
	}
	if cfg == nil || cfg.APIKey != "" {
		t.Fatalf("expected empty config for missing file, got %+v", cfg)
	}
}
