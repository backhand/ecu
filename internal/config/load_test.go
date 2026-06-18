package config

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		Image:                 "ecu-snap",                        // C7: pre-baked snapshot NAME
		ContainerImage:        "ghcr.io/backhand/ecu-image:v1.2", // C7: container image ref (distinct)
		MaxSessions:           5,
		IdleTimeout:           45 * time.Minute,
		MaxLifetime:           4 * time.Hour,
		BakeTimeout:           25 * time.Minute, // C7
		MaxPersistentSessions: 2,
		PersistentMaxLifetime: 18 * time.Hour,     // C8
		PersistentMaxAge:      5 * 24 * time.Hour, // C8
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
		reloaded.Image != original.Image ||
		reloaded.ContainerImage != original.ContainerImage ||
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
	if reloaded.BakeTimeout != original.BakeTimeout {
		t.Fatalf("BakeTimeout = %v, want %v", reloaded.BakeTimeout, original.BakeTimeout)
	}
	if reloaded.PersistentMaxLifetime != original.PersistentMaxLifetime {
		t.Fatalf("PersistentMaxLifetime = %v, want %v", reloaded.PersistentMaxLifetime, original.PersistentMaxLifetime)
	}
	if reloaded.PersistentMaxAge != original.PersistentMaxAge {
		t.Fatalf("PersistentMaxAge = %v, want %v", reloaded.PersistentMaxAge, original.PersistentMaxAge)
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

// stubHetznerAPI points hetznerDatacentersURL at a test server returning the
// given JSON for the duration of the test, restoring the real URL afterward. A
// nil body means "no server" (left as-is for offline behavior); callers that
// want the validation to stay silent should pass a 401-style server or rely on
// the type-name not resolving.
func stubHetznerAPI(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	saved := hetznerDatacentersURL
	hetznerDatacentersURL = srv.URL
	t.Cleanup(func() { hetznerDatacentersURL = saved })
}

// availableComboBody is a /v1/datacenters payload where server type "cpx32"
// (id 7) IS available in location "nbg1" — so the wizard validation stays
// silent. It mirrors the real response shape closely enough for the parser.
const availableComboBody = `{
  "datacenters": [
    {"location": {"name": "nbg1"}, "server_types": {"available": [3, 7, 9]}}
  ],
  "server_types": [
    {"id": 3, "name": "cpx21"},
    {"id": 7, "name": "cpx32"},
    {"id": 9, "name": "cpx41"}
  ]
}`

// TestRunWizardPromptsAndPersistsTypeAndRegion drives runWizardInto with a
// scripted reader for the full production-hetzner required set and verifies it
// (a) prompts for instance_type and region, (b) records the scripted answers on
// the Config, and (c) round-trips them through the on-disk config file so a
// later headless boot finds them. The optional Hetzner validation is stubbed to
// an available combo so it stays silent and hermetic (no real network).
func TestRunWizardPromptsAndPersistsTypeAndRegion(t *testing.T) {
	stubHetznerAPI(t, http.StatusOK, availableComboBody)

	// Production-hetzner mode: no dev tool-server, default provider. cfg arrives
	// from resolve with everything required still empty.
	cfg := &Config{Provider: "hetzner"}

	// Scripted answers, in prompt order: API key, Hetzner token, server type,
	// region, hostname (optional — left blank).
	script := strings.Join([]string{
		"k_admin_live",
		"hc_token_live",
		"cpx32",
		"nbg1",
		"", // hostname: just press enter
	}, "\n") + "\n"

	var out strings.Builder
	if err := runWizardInto(cfg, strings.NewReader(script), &out, requiredKeys); err != nil {
		t.Fatalf("runWizardInto returned error: %v", err)
	}

	// The prompts must mention the two new settings so an operator knows what to
	// type.
	prompts := out.String()
	if !strings.Contains(prompts, "ECU_INSTANCE_TYPE") {
		t.Fatalf("wizard output did not prompt for ECU_INSTANCE_TYPE:\n%s", prompts)
	}
	if !strings.Contains(prompts, "ECU_REGION") {
		t.Fatalf("wizard output did not prompt for ECU_REGION:\n%s", prompts)
	}

	// The scripted answers must land on the config.
	if cfg.APIKey != "k_admin_live" {
		t.Fatalf("cfg.APIKey = %q, want scripted value", cfg.APIKey)
	}
	if cfg.HCloudToken != "hc_token_live" {
		t.Fatalf("cfg.HCloudToken = %q, want scripted value", cfg.HCloudToken)
	}
	if cfg.InstanceType != "cpx32" {
		t.Fatalf("cfg.InstanceType = %q, want cpx32", cfg.InstanceType)
	}
	if cfg.Region != "nbg1" {
		t.Fatalf("cfg.Region = %q, want nbg1", cfg.Region)
	}

	// Round-trip through disk: write the wizard result and reload it; type +
	// region must survive (fileConfig <-> Config).
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeFileConfig(path, cfg); err != nil {
		t.Fatalf("writeFileConfig: %v", err)
	}
	reloaded, err := readFileConfig(path)
	if err != nil {
		t.Fatalf("readFileConfig: %v", err)
	}
	if reloaded.InstanceType != "cpx32" || reloaded.Region != "nbg1" {
		t.Fatalf("type/region did not round-trip: got %q/%q, want cpx32/nbg1",
			reloaded.InstanceType, reloaded.Region)
	}

	// And the reloaded file must satisfy the production-hetzner required set with
	// no further prompting (resolve over the file alone, no env, no TTY).
	resolved, runWizard, err := resolve(map[string]string{}, reloaded, false /*isTTY*/, requiredKeys)
	if err != nil {
		t.Fatalf("resolve over the persisted wizard config failed: %v", err)
	}
	if runWizard {
		t.Fatalf("runWizard = true after persisting wizard answers; want a clean headless boot")
	}
	if resolved.InstanceType != "cpx32" || resolved.Region != "nbg1" {
		t.Fatalf("resolved type/region = %q/%q, want cpx32/nbg1", resolved.InstanceType, resolved.Region)
	}
}

// TestWarnIfHetznerComboUnavailableWarns verifies the optional validation emits
// a warning when the chosen server type is provably NOT available in the chosen
// location (the type exists in the catalog but is absent from that location's
// available set).
func TestWarnIfHetznerComboUnavailableWarns(t *testing.T) {
	// cpx32 (id 7) exists, location fsn1 is known, but fsn1 offers only id 3 — so
	// cpx32 is unavailable there and the validation should warn. Mirrors the live
	// finding that fsn1 had no x86 4GB+ stock.
	body := `{
      "datacenters": [
        {"location": {"name": "fsn1"}, "server_types": {"available": [3]}}
      ],
      "server_types": [
        {"id": 3, "name": "cpx21"},
        {"id": 7, "name": "cpx32"}
      ]
    }`
	stubHetznerAPI(t, http.StatusOK, body)

	var out strings.Builder
	warnIfHetznerComboUnavailable(&out, "hc_tok", "cpx32", "fsn1")
	if !strings.Contains(out.String(), "cpx32") || !strings.Contains(out.String(), "fsn1") {
		t.Fatalf("expected an unavailability warning naming cpx32/fsn1, got: %q", out.String())
	}
}

// TestWarnIfHetznerComboUnavailableSilentOnAvailable verifies the validation
// stays SILENT when the combo is available, and also when the API errors (it is
// best-effort: a non-200 must never produce a warning).
func TestWarnIfHetznerComboUnavailableSilentOnAvailable(t *testing.T) {
	// Available combo: no warning.
	stubHetznerAPI(t, http.StatusOK, availableComboBody)
	var out strings.Builder
	warnIfHetznerComboUnavailable(&out, "hc_tok", "cpx32", "nbg1")
	if out.Len() != 0 {
		t.Fatalf("expected silence for an available combo, got: %q", out.String())
	}

	// API error (e.g. bad token -> 401): still silent, never blocks setup.
	stubHetznerAPI(t, http.StatusUnauthorized, `{"error":{"code":"unauthorized"}}`)
	var out2 strings.Builder
	warnIfHetznerComboUnavailable(&out2, "bad_tok", "cpx32", "nbg1")
	if out2.Len() != 0 {
		t.Fatalf("expected silence on API error, got: %q", out2.String())
	}
}
