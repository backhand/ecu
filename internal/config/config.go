// Package config loads ECU control-plane configuration from environment
// variables and an optional JSON config file, applying a single precedence
// rule: environment variable > config-file value > built-in default.
//
// The decision of what to do when required configuration is missing — boot
// headless, run the interactive wizard, or fail fast — is isolated in the pure
// function resolve, which takes its inputs (env map, file config, TTY flag) as
// arguments so it can be unit-tested without a real terminal. Load wires the
// real environment, the on-disk config file, and real TTY detection into
// resolve.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default values for optional settings. These apply only when neither the
// environment nor the config file supplies a value.
const (
	defaultListen                = "127.0.0.1:8080"
	defaultProvider              = "hetzner"
	defaultMaxPersistentSessions = 3
	defaultDBPath                = "~/.local/share/ecu/ecu.db"
	defaultConfigPath            = "~/.config/ecu/config.json"
	// defaultProvisionTimeout bounds how long the control plane waits for a
	// freshly provisioned instance's agent to register before tearing the
	// instance down (C4). A cold boot pulls a container image, so minutes.
	defaultProvisionTimeout = 5 * time.Minute
	// defaultBaseImage is the cold-boot base OS image used when no pre-baked
	// image is configured.
	defaultBaseImage = "ubuntu-24.04"
	// defaultContainerImage is the container (Docker) image cloud-init pulls and
	// runs on the instance, distinct from the pre-baked snapshot NAME (Image /
	// ECU_IMAGE). It is what a bake pulls and what a session runs.
	defaultContainerImage = "ghcr.io/backhand/ecu-image:latest"
	// defaultBakeTimeout bounds how long the C7 pre-bake waits for the bake
	// instance to finish pulling the (multi-GB) container image and call back
	// before tearing the bake instance down. Generous on purpose — a cold pull
	// over a slow link is minutes.
	defaultBakeTimeout = 20 * time.Minute
	// C5 reaper / cap defaults. They apply when neither env nor file supplies a
	// value. defaultMaxSessions is the global active-session cap; explicitly
	// setting ECU_MAX_SESSIONS=0 disables it (unlimited). defaultIdleTimeout and
	// defaultMaxLifetime bound an idle and a maximally-old session respectively;
	// defaultReapInterval is the base sweep cadence (clamped down to the
	// smallest positive timeout by the reaper).
	defaultMaxSessions  = 10
	defaultIdleTimeout  = 15 * time.Minute
	defaultMaxLifetime  = 8 * time.Hour
	defaultReapInterval = 1 * time.Minute
)

// Config holds every ECU_* setting the control plane understands. All fields
// are defined here even though Component 2 only consumes a subset (Listen,
// APIKey, DBPath, and the DevToolServer dev seam); the rest are modeled now so
// later components (provisioning, reaper, persistence) can read them without a
// schema change.
type Config struct {
	// Hostname is the public hostname used for TLS (ECU_HOSTNAME). Consumed by
	// the TLS/packaging component (C10), stored now.
	Hostname string

	// Listen is the address the control-plane HTTP server binds to
	// (ECU_LISTEN). Defaults to defaultListen.
	Listen string

	// APIKey is the bootstrap admin API key (ECU_API_KEY). It is the only
	// required setting for the Component 2 skeleton; it is seeded into the
	// store as an active admin key on startup.
	APIKey string

	// Provider selects the cloud provider implementation (ECU_PROVIDER).
	// Defaults to defaultProvider. Consumed by the provider component (C4).
	Provider string

	// HCloudToken is the Hetzner Cloud API token (ECU_HCLOUD_TOKEN). Consumed
	// by the Hetzner provider (C4).
	HCloudToken string

	// InstanceType is the provider instance type/size (ECU_INSTANCE_TYPE).
	// Consumed by the provider component (C4).
	InstanceType string

	// Region is the provider region (ECU_REGION). Consumed by C4.
	Region string

	// Image is the pre-baked provider SNAPSHOT NAME (ECU_IMAGE). It controls C7
	// pre-baking: when set, the control plane looks up (or auto-builds) a
	// provider snapshot under this name and boots sessions from it. It is NOT a
	// container/Docker image ref — see ContainerImage for that. Empty disables
	// pre-baking (sessions cold-boot from BaseImage).
	Image string

	// ContainerImage is the container (Docker) image ref that cloud-init pulls
	// and `docker run`s on the instance (ECU_CONTAINER_IMAGE), e.g.
	// "ghcr.io/backhand/ecu-image:latest". This is a DISTINCT concept from Image
	// (ECU_IMAGE, the pre-baked snapshot name): a bake instance pulls THIS image
	// to produce the snapshot, and every session runs THIS image. Defaults to
	// defaultContainerImage.
	ContainerImage string

	// BaseImage is the cold-boot base OS image (ECU_BASE_IMAGE), used by C4
	// provisioning when no pre-baked image is available. Distinct from Image
	// (the pre-baked snapshot name). Defaults to defaultBaseImage.
	BaseImage string

	// BakeTimeout bounds how long C7 pre-baking waits for the bake instance to
	// pull the container image and call back before tearing it down
	// (ECU_BAKE_TIMEOUT). Defaults to defaultBakeTimeout.
	BakeTimeout time.Duration

	// AgentBinaryURL is where the instance fetches the `ecu` binary from
	// (ECU_AGENT_BINARY_URL), injected into cloud-init (C4). C10 supplies real
	// arch-specific release URLs.
	AgentBinaryURL string

	// ProvisionTimeout bounds how long the control plane waits for a
	// newly-provisioned instance's agent to register before tearing it down
	// (ECU_PROVISION_TIMEOUT). Defaults to defaultProvisionTimeout.
	ProvisionTimeout time.Duration

	// MaxSessions is the global cap on concurrently ACTIVE
	// (provisioning+ready) sessions (ECU_MAX_SESSIONS), enforced by POST
	// /sessions (C5). Defaults to defaultMaxSessions. Explicitly setting
	// ECU_MAX_SESSIONS=0 DISABLES the cap (unlimited).
	MaxSessions int

	// IdleTimeout is the inactivity timeout before a session is reaped
	// (ECU_IDLE_TIMEOUT). Defaults to defaultIdleTimeout; 0 disables idle
	// reaping. Enforced by C5.
	IdleTimeout time.Duration

	// MaxLifetime is the hard ceiling on session lifetime regardless of
	// activity (ECU_MAX_LIFETIME). Defaults to defaultMaxLifetime; 0 disables
	// lifetime reaping. Enforced by C5.
	MaxLifetime time.Duration

	// ReapInterval is the base cadence of the C5 reaper's sweep
	// (ECU_REAP_INTERVAL). Defaults to defaultReapInterval. The reaper clamps it
	// down so it never exceeds the smallest positive idle/lifetime timeout.
	ReapInterval time.Duration

	// MaxPersistentSessions caps concurrent persistent sessions
	// (ECU_MAX_PERSISTENT_SESSIONS). Defaults to defaultMaxPersistentSessions.
	// Enforced by the persistence component (C8).
	MaxPersistentSessions int

	// DBPath is the filesystem path to the embedded SQLite database (ECU_DB).
	// Defaults to defaultDBPath. A leading ~ is expanded to the user's home
	// directory.
	DBPath string

	// ConfigPath is the path to the JSON config file (ECU_CONFIG). Defaults to
	// defaultConfigPath. A leading ~ is expanded.
	ConfigPath string

	// DevToolServer is a DEV-ONLY seam (ECU_DEV_TOOLSERVER): a single
	// tool-server base URL (e.g. http://127.0.0.1:8000) that the control plane
	// proxies every session to, bypassing real provisioning and tunneling.
	// When set, POST /sessions marks sessions ready immediately and points them
	// at this URL. This exists purely so Component 2 is runnable end-to-end
	// against a local Component-1 tool server; Components 3 (tunnel) and 4
	// (provider) supersede it and it is not used in production.
	DevToolServer string
}

// fileConfig is the JSON shape persisted to and reloaded from the config file.
// It mirrors Config but stores durations as strings (Go duration syntax) and
// uses omitempty so an unset field round-trips as absent rather than a zero
// value. resolve and the wizard convert between fileConfig and Config.
type fileConfig struct {
	Hostname              string `json:"hostname,omitempty"`
	Listen                string `json:"listen,omitempty"`
	APIKey                string `json:"api_key,omitempty"`
	Provider              string `json:"provider,omitempty"`
	HCloudToken           string `json:"hcloud_token,omitempty"`
	InstanceType          string `json:"instance_type,omitempty"`
	Region                string `json:"region,omitempty"`
	Image                 string `json:"image,omitempty"`
	ContainerImage        string `json:"container_image,omitempty"`
	BaseImage             string `json:"base_image,omitempty"`
	AgentBinaryURL        string `json:"agent_binary_url,omitempty"`
	ProvisionTimeout      string `json:"provision_timeout,omitempty"`
	BakeTimeout           string `json:"bake_timeout,omitempty"`
	MaxSessions           int    `json:"max_sessions,omitempty"`
	IdleTimeout           string `json:"idle_timeout,omitempty"`
	MaxLifetime           string `json:"max_lifetime,omitempty"`
	ReapInterval          string `json:"reap_interval,omitempty"`
	MaxPersistentSessions int    `json:"max_persistent_sessions,omitempty"`
	DBPath                string `json:"db_path,omitempty"`
	DevToolServer         string `json:"dev_tool_server,omitempty"`
}

// requiredKeys lists the ECU_* settings that must be present (via env or file)
// for the Component 2 skeleton to boot. Only the bootstrap admin key is
// required here: the DB has a default and provisioning is not wired up yet.
var requiredKeys = []string{"ECU_API_KEY"}

// expandHome expands a leading "~" or "~/" in a path to the user's home
// directory. Other paths are returned unchanged. An empty path is returned
// as-is so callers can distinguish "unset" from a real path.
func expandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		// e.g. "~user" — not supported; leave untouched.
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// pick returns the first non-empty string among the supplied candidates,
// applied in precedence order (env, then file, then default).
func pick(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}

// parseIntSetting parses an integer ECU_* value, preferring env over file. The
// file value arrives already decoded as an int (fileSet reports whether the
// file actually supplied it). An empty/unset env value with no file value
// yields def with no error; a set-but-invalid env value errors.
func parseIntSetting(name, envVal string, fileVal int, fileSet bool, def int) (int, error) {
	if envVal != "" {
		n, err := strconv.Atoi(strings.TrimSpace(envVal))
		if err != nil {
			return 0, fmt.Errorf("%s: invalid integer %q: %w", name, envVal, err)
		}
		return n, nil
	}
	if fileSet {
		return fileVal, nil
	}
	return def, nil
}

// parseDurationSetting parses a Go duration ECU_* value (e.g. "30m", "2h"),
// preferring env over file. Empty/unset yields the zero duration with no error;
// a set-but-invalid value errors.
func parseDurationSetting(name, envVal, fileVal string) (time.Duration, error) {
	if envVal != "" {
		d, err := time.ParseDuration(strings.TrimSpace(envVal))
		if err != nil {
			return 0, fmt.Errorf("%s: invalid duration %q: %w", name, envVal, err)
		}
		return d, nil
	}
	if fileVal != "" {
		d, err := time.ParseDuration(strings.TrimSpace(fileVal))
		if err != nil {
			return 0, fmt.Errorf("%s (config file): invalid duration %q: %w", name, fileVal, err)
		}
		return d, nil
	}
	return 0, nil
}

// resolve merges env over file-config over defaults, then decides what to do.
// isTTY is injected so the decision is unit-testable without a real terminal.
// Returns the resolved config, whether the wizard should run, and an error
// (the fail-fast error names exactly the missing required keys).
//
// Precedence for every value is: env var > config-file value > default.
//
// Decision rule once values are merged:
//   - all required keys present                  -> runWizard=false, no error
//   - a required key missing AND isTTY            -> runWizard=true,  no error
//   - a required key missing AND NOT isTTY        -> error listing the missing keys
func resolve(env map[string]string, fileCfg *Config, isTTY bool, required []string) (cfg *Config, runWizard bool, err error) {
	if fileCfg == nil {
		fileCfg = &Config{}
	}

	c := &Config{}

	// String settings: env > file > default.
	c.Hostname = pick(env["ECU_HOSTNAME"], fileCfg.Hostname)
	c.Listen = pick(env["ECU_LISTEN"], fileCfg.Listen, defaultListen)
	c.APIKey = pick(env["ECU_API_KEY"], fileCfg.APIKey)
	c.Provider = pick(env["ECU_PROVIDER"], fileCfg.Provider, defaultProvider)
	c.HCloudToken = pick(env["ECU_HCLOUD_TOKEN"], fileCfg.HCloudToken)
	c.InstanceType = pick(env["ECU_INSTANCE_TYPE"], fileCfg.InstanceType)
	c.Region = pick(env["ECU_REGION"], fileCfg.Region)
	c.Image = pick(env["ECU_IMAGE"], fileCfg.Image)
	c.ContainerImage = pick(env["ECU_CONTAINER_IMAGE"], fileCfg.ContainerImage, defaultContainerImage)
	c.BaseImage = pick(env["ECU_BASE_IMAGE"], fileCfg.BaseImage, defaultBaseImage)
	c.AgentBinaryURL = pick(env["ECU_AGENT_BINARY_URL"], fileCfg.AgentBinaryURL)
	c.DevToolServer = pick(env["ECU_DEV_TOOLSERVER"], fileCfg.DevToolServer)

	// Integer settings. MaxSessions defaults to defaultMaxSessions when neither
	// env nor file supplies it; an explicit ECU_MAX_SESSIONS=0 is honored as
	// "unlimited" (parseIntSetting returns the parsed env value, so 0 means 0
	// here, which the cap treats as no limit).
	if c.MaxSessions, err = parseIntSetting(
		"ECU_MAX_SESSIONS", env["ECU_MAX_SESSIONS"], fileCfg.MaxSessions, fileCfg.MaxSessions != 0, defaultMaxSessions,
	); err != nil {
		return nil, false, err
	}
	if c.MaxPersistentSessions, err = parseIntSetting(
		"ECU_MAX_PERSISTENT_SESSIONS", env["ECU_MAX_PERSISTENT_SESSIONS"], fileCfg.MaxPersistentSessions,
		fileCfg.MaxPersistentSessions != 0, defaultMaxPersistentSessions,
	); err != nil {
		return nil, false, err
	}

	// Duration settings. File values come in as already-parsed durations, so
	// stringify them for the shared parser.
	var fileIdle, fileMaxLife, fileReapInterval string
	if fileCfg.IdleTimeout != 0 {
		fileIdle = fileCfg.IdleTimeout.String()
	}
	if fileCfg.MaxLifetime != 0 {
		fileMaxLife = fileCfg.MaxLifetime.String()
	}
	if fileCfg.ReapInterval != 0 {
		fileReapInterval = fileCfg.ReapInterval.String()
	}
	if c.IdleTimeout, err = parseDurationSetting("ECU_IDLE_TIMEOUT", env["ECU_IDLE_TIMEOUT"], fileIdle); err != nil {
		return nil, false, err
	}
	if c.MaxLifetime, err = parseDurationSetting("ECU_MAX_LIFETIME", env["ECU_MAX_LIFETIME"], fileMaxLife); err != nil {
		return nil, false, err
	}
	if c.ReapInterval, err = parseDurationSetting("ECU_REAP_INTERVAL", env["ECU_REAP_INTERVAL"], fileReapInterval); err != nil {
		return nil, false, err
	}
	var fileProvTimeout string
	if fileCfg.ProvisionTimeout != 0 {
		fileProvTimeout = fileCfg.ProvisionTimeout.String()
	}
	if c.ProvisionTimeout, err = parseDurationSetting("ECU_PROVISION_TIMEOUT", env["ECU_PROVISION_TIMEOUT"], fileProvTimeout); err != nil {
		return nil, false, err
	}
	var fileBakeTimeout string
	if fileCfg.BakeTimeout != 0 {
		fileBakeTimeout = fileCfg.BakeTimeout.String()
	}
	if c.BakeTimeout, err = parseDurationSetting("ECU_BAKE_TIMEOUT", env["ECU_BAKE_TIMEOUT"], fileBakeTimeout); err != nil {
		return nil, false, err
	}

	// Apply duration defaults where unset (zero). A zero IdleTimeout/MaxLifetime
	// would DISABLE those reaper rules; operators expect the documented defaults
	// unless they deliberately override, so we fill them here. (To truly disable
	// a rule, an operator must set it elsewhere in code, not via config — the
	// product wants a default idle/lifetime ceiling.)
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.MaxLifetime == 0 {
		c.MaxLifetime = defaultMaxLifetime
	}
	if c.ReapInterval == 0 {
		c.ReapInterval = defaultReapInterval
	}
	if c.ProvisionTimeout == 0 {
		c.ProvisionTimeout = defaultProvisionTimeout
	}
	if c.BakeTimeout == 0 {
		c.BakeTimeout = defaultBakeTimeout
	}

	// Path settings: env > file > default, then expand ~.
	c.DBPath = pick(env["ECU_DB"], fileCfg.DBPath, defaultDBPath)
	c.ConfigPath = pick(env["ECU_CONFIG"], fileCfg.ConfigPath, defaultConfigPath)
	if c.DBPath, err = expandHome(c.DBPath); err != nil {
		return nil, false, err
	}
	if c.ConfigPath, err = expandHome(c.ConfigPath); err != nil {
		return nil, false, err
	}

	// Decide based on required keys. The caller passes the BASE required set
	// (package-level requiredKeys, i.e. ECU_API_KEY); we augment it from the
	// merged config so requirements are context-dependent: a production
	// deployment (no ECU_DEV_TOOLSERVER) on the hetzner provider also needs
	// ECU_HCLOUD_TOKEN. In dev-toolserver mode, or with a non-hetzner provider,
	// only the base set applies. We evaluate "present" against the merged
	// config so a value supplied by the file counts as present.
	required = requiredFor(c, required)
	missing := missingRequired(c, required)
	if len(missing) == 0 {
		return c, false, nil
	}
	if isTTY {
		// A human is attached: collect the missing values interactively.
		return c, true, nil
	}
	// No TTY and required config is missing: fail fast with a precise message.
	return nil, false, fmt.Errorf("missing required configuration: %s (set via environment, e.g. %s, or a config file)",
		strings.Join(missing, ", "), missing[0])
}

// requiredFor returns the effective required-key set: the supplied base set
// plus any keys implied by the merged config. The only context-dependent
// requirement today is the provider token: a production deployment (no
// ECU_DEV_TOOLSERVER) using the hetzner provider must supply ECU_HCLOUD_TOKEN.
// The dev-toolserver path provisions nothing, so it stays at the base set; a
// non-hetzner provider has no token requirement here. The base set is never
// dropped, so a caller asserting "only ECU_API_KEY is required" in dev mode
// still holds.
func requiredFor(c *Config, base []string) []string {
	out := append([]string(nil), base...)
	if c.DevToolServer == "" && strings.EqualFold(c.Provider, "hetzner") {
		if !contains(out, "ECU_HCLOUD_TOKEN") {
			out = append(out, "ECU_HCLOUD_TOKEN")
		}
	}
	return out
}

// contains reports whether s is in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// missingRequired returns the subset of required keys that have no value in the
// merged config, in a stable (sorted) order so error messages are
// deterministic.
func missingRequired(c *Config, required []string) []string {
	var missing []string
	for _, key := range required {
		if valueForKey(c, key) == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// valueForKey returns the merged string value backing a required ECU_* key.
// Only keys that can appear in requiredKeys need to be handled.
func valueForKey(c *Config, key string) string {
	switch key {
	case "ECU_API_KEY":
		return c.APIKey
	case "ECU_HOSTNAME":
		return c.Hostname
	case "ECU_HCLOUD_TOKEN":
		return c.HCloudToken
	case "ECU_PROVIDER":
		return c.Provider
	default:
		return ""
	}
}
