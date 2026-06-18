package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Load reads configuration from the environment and the on-disk config file,
// merges them with built-in defaults (env > file > default), and decides how
// to proceed:
//
//   - If all required settings are present, it returns the resolved config and
//     the control plane boots headless.
//   - If a required setting is missing and stdin is a TTY, it runs the
//     interactive wizard, persists the answers to the config file, and returns
//     the completed config.
//   - If a required setting is missing and stdin is not a TTY, it fails fast
//     with an error naming exactly the missing keys (it never blocks on stdin).
//
// TTY detection uses the stdlib stat approach
// ((os.Stdin.Stat().Mode() & os.ModeCharDevice) != 0); no third-party isatty
// dependency is introduced.
func Load() (*Config, error) {
	env := envMap()

	// Determine the config-file path up front (env > default) so we can read an
	// existing file before resolving. A missing file is tolerated.
	cfgPath, err := expandHome(pick(env["ECU_CONFIG"], defaultConfigPath))
	if err != nil {
		return nil, err
	}
	fileCfg, err := readFileConfig(cfgPath)
	if err != nil {
		return nil, err
	}

	cfg, runWizard, err := resolve(env, fileCfg, stdinIsTTY(), requiredKeys)
	if err != nil {
		return nil, err
	}
	if !runWizard {
		return cfg, nil
	}

	// Interactive path: prompt for the missing required values, persist them,
	// and return the completed config. cfg already carries the merged
	// non-required values (defaults, file, env).
	if err := runWizardInto(cfg, os.Stdin, os.Stdout, requiredKeys); err != nil {
		return nil, err
	}
	cfg.ConfigPath = cfgPath
	if err := writeFileConfig(cfgPath, cfg); err != nil {
		return nil, fmt.Errorf("persisting config to %s: %w", cfgPath, err)
	}
	return cfg, nil
}

// envMap snapshots the process environment into a map for resolve. Only the
// ECU_* keys matter, but copying everything is cheap and keeps the seam simple.
func envMap() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// stdinIsTTY reports whether standard input is connected to an interactive
// terminal, using only the standard library.
//
// The base test is the stdlib stat approach: a terminal is a character device,
// so (mode & os.ModeCharDevice) != 0. But that bit is also set for the null
// device (/dev/null on Unix, NUL on Windows), which is exactly what a container
// or k3s pod with no TTY allocated gets as stdin. Treating /dev/null as a
// terminal would send the no-TTY deploy into the interactive wizard and block on
// stdin — the precise hang the fail-fast rule exists to prevent. So we
// additionally exclude the null device by identity (same file as os.DevNull),
// keeping the decision dependency-free while making the no-TTY path fail fast.
func stdinIsTTY() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		// Pipe, regular file, socket: not interactive.
		return false
	}
	return !isNullDevice(info)
}

// isNullDevice reports whether the stat result refers to the platform null
// device (e.g. /dev/null). It compares against a fresh stat of os.DevNull using
// os.SameFile, which matches on the underlying device/inode rather than on a
// path string, so it is robust regardless of how stdin was wired up.
func isNullDevice(info os.FileInfo) bool {
	nullInfo, err := os.Stat(os.DevNull)
	if err != nil {
		return false
	}
	return os.SameFile(info, nullInfo)
}

// readFileConfig loads and decodes the JSON config file at path into a Config.
// A non-existent file is not an error: it returns an empty *Config so resolve
// treats every file value as unset. Durations stored as strings are parsed back
// into time.Duration.
func readFileConfig(path string) (*Config, error) {
	if path == "" {
		return &Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	return fileConfigToConfig(&fc)
}

// fileConfigToConfig converts the persisted JSON shape into a Config, parsing
// the string durations. A malformed duration in the file is reported clearly.
func fileConfigToConfig(fc *fileConfig) (*Config, error) {
	c := &Config{
		Hostname:              fc.Hostname,
		Listen:                fc.Listen,
		APIKey:                fc.APIKey,
		Provider:              fc.Provider,
		HCloudToken:           fc.HCloudToken,
		InstanceType:          fc.InstanceType,
		Region:                fc.Region,
		Image:                 fc.Image,
		ContainerImage:        fc.ContainerImage,
		BaseImage:             fc.BaseImage,
		AgentBinaryURL:        fc.AgentBinaryURL,
		MaxSessions:           fc.MaxSessions,
		MaxPersistentSessions: fc.MaxPersistentSessions,
		DBPath:                fc.DBPath,
		DevToolServer:         fc.DevToolServer,
	}
	if fc.ProvisionTimeout != "" {
		d, err := time.ParseDuration(fc.ProvisionTimeout)
		if err != nil {
			return nil, fmt.Errorf("config file provision_timeout: invalid duration %q: %w", fc.ProvisionTimeout, err)
		}
		c.ProvisionTimeout = d
	}
	if fc.BakeTimeout != "" {
		d, err := time.ParseDuration(fc.BakeTimeout)
		if err != nil {
			return nil, fmt.Errorf("config file bake_timeout: invalid duration %q: %w", fc.BakeTimeout, err)
		}
		c.BakeTimeout = d
	}
	if fc.IdleTimeout != "" {
		d, err := time.ParseDuration(fc.IdleTimeout)
		if err != nil {
			return nil, fmt.Errorf("config file idle_timeout: invalid duration %q: %w", fc.IdleTimeout, err)
		}
		c.IdleTimeout = d
	}
	if fc.MaxLifetime != "" {
		d, err := time.ParseDuration(fc.MaxLifetime)
		if err != nil {
			return nil, fmt.Errorf("config file max_lifetime: invalid duration %q: %w", fc.MaxLifetime, err)
		}
		c.MaxLifetime = d
	}
	if fc.ReapInterval != "" {
		d, err := time.ParseDuration(fc.ReapInterval)
		if err != nil {
			return nil, fmt.Errorf("config file reap_interval: invalid duration %q: %w", fc.ReapInterval, err)
		}
		c.ReapInterval = d
	}
	return c, nil
}

// writeFileConfig persists cfg to path as JSON, creating the parent directory
// if needed. Durations are stored using Go duration syntax; omitempty keeps
// unset fields out of the file so it round-trips cleanly on reload.
func writeFileConfig(path string, cfg *Config) error {
	if path == "" {
		return fmt.Errorf("empty config path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating config dir %s: %w", dir, err)
		}
	}
	fc := configToFileConfig(cfg)
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file may contain the bootstrap API key and provider token.
	return os.WriteFile(path, data, 0o600)
}

// configToFileConfig converts a Config into the persisted JSON shape,
// stringifying durations.
func configToFileConfig(cfg *Config) *fileConfig {
	fc := &fileConfig{
		Hostname:              cfg.Hostname,
		Listen:                cfg.Listen,
		APIKey:                cfg.APIKey,
		Provider:              cfg.Provider,
		HCloudToken:           cfg.HCloudToken,
		InstanceType:          cfg.InstanceType,
		Region:                cfg.Region,
		Image:                 cfg.Image,
		ContainerImage:        cfg.ContainerImage,
		BaseImage:             cfg.BaseImage,
		AgentBinaryURL:        cfg.AgentBinaryURL,
		MaxSessions:           cfg.MaxSessions,
		MaxPersistentSessions: cfg.MaxPersistentSessions,
		DBPath:                cfg.DBPath,
		DevToolServer:         cfg.DevToolServer,
	}
	if cfg.IdleTimeout != 0 {
		fc.IdleTimeout = cfg.IdleTimeout.String()
	}
	if cfg.MaxLifetime != 0 {
		fc.MaxLifetime = cfg.MaxLifetime.String()
	}
	if cfg.ReapInterval != 0 {
		fc.ReapInterval = cfg.ReapInterval.String()
	}
	if cfg.ProvisionTimeout != 0 {
		fc.ProvisionTimeout = cfg.ProvisionTimeout.String()
	}
	if cfg.BakeTimeout != 0 {
		fc.BakeTimeout = cfg.BakeTimeout.String()
	}
	return fc
}

// runWizardInto prompts the operator for any missing required settings, reading
// answers from in and writing prompts to out. It is intentionally minimal and
// robust: it reads line-by-line with a bufio.Scanner and re-prompts on empty
// input for a required key. The reader is injected so the wizard can be driven
// in tests, though Load wires os.Stdin.
//
// The base required key is ECU_API_KEY; a production deployment on the hetzner
// provider additionally requires ECU_HCLOUD_TOKEN, so the wizard augments the
// passed-in base set with requiredFor (the same context-dependent computation
// resolve uses) and prompts for the token when it is required and missing. We
// additionally offer to capture the public hostname since it is harmless and
// convenient, but never require it.
func runWizardInto(cfg *Config, in io.Reader, out io.Writer, required []string) error {
	scanner := bufio.NewScanner(in)
	fmt.Fprintln(out, "ECU control plane — initial setup")
	fmt.Fprintln(out, "Required configuration is missing; let's fill it in.")
	fmt.Fprintln(out)

	// Mirror resolve's context-dependent requirement so the wizard prompts for
	// exactly what resolve would have flagged as missing.
	required = requiredFor(cfg, required)
	requiredSet := make(map[string]bool, len(required))
	for _, k := range required {
		requiredSet[k] = true
	}

	// Bootstrap admin API key (required).
	if requiredSet["ECU_API_KEY"] && cfg.APIKey == "" {
		val, err := promptRequired(scanner, out, "Bootstrap admin API key (ECU_API_KEY)")
		if err != nil {
			return err
		}
		cfg.APIKey = val
	}

	// Hetzner Cloud API token (required in production on the hetzner provider).
	// It is a secret, but reading it as a normal line keeps the wizard
	// dependency-free; mirrors the API-key prompt.
	if requiredSet["ECU_HCLOUD_TOKEN"] && cfg.HCloudToken == "" {
		val, err := promptRequired(scanner, out, "Hetzner Cloud API token (ECU_HCLOUD_TOKEN)")
		if err != nil {
			return err
		}
		cfg.HCloudToken = val
	}

	// Public hostname (optional, prompted for convenience).
	if cfg.Hostname == "" {
		val, err := promptOptional(scanner, out, "Public hostname for TLS (ECU_HOSTNAME, optional)")
		if err != nil {
			return err
		}
		cfg.Hostname = val
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Setup complete; configuration will be saved.")
	return nil
}

// promptRequired prints prompt and reads a non-empty trimmed line, re-prompting
// until one is supplied or input ends.
func promptRequired(scanner *bufio.Scanner, out io.Writer, prompt string) (string, error) {
	for {
		fmt.Fprintf(out, "%s: ", prompt)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			return "", fmt.Errorf("input closed before %q was provided", prompt)
		}
		if v := strings.TrimSpace(scanner.Text()); v != "" {
			return v, nil
		}
		fmt.Fprintln(out, "  (a value is required)")
	}
}

// promptOptional prints prompt and reads a single trimmed line, returning "" if
// the operator just presses enter.
func promptOptional(scanner *bufio.Scanner, out io.Writer, prompt string) (string, error) {
	fmt.Fprintf(out, "%s: ", prompt)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", nil
	}
	return strings.TrimSpace(scanner.Text()), nil
}
