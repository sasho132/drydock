// Package config loads and validates drydock's configuration from a YAML
// file, then layers environment variables and command-line overrides on top.
//
// Precedence (lowest to highest):
//
//	defaults < config file < environment < command-line flags
//
// Per-container overrides live in container labels and are resolved by the
// policy package.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "6h" or
// integers interpreted as seconds.
type Duration struct{ time.Duration }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw any
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case int:
		d.Duration = time.Duration(v) * time.Second
	case float64:
		d.Duration = time.Duration(v * float64(time.Second))
	case string:
		parsed, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		d.Duration = parsed
	default:
		return fmt.Errorf("invalid duration value %v", raw)
	}
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// Auth holds registry credentials.
type Auth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Token is an identity token (for example a registry access token) used
	// instead of username/password.
	Token string `yaml:"token"`
}

// Notification configures one outbound notification sink.
type Notification struct {
	// Type is "webhook", "slack", or "json".
	Type string `yaml:"type"`
	// URL is the destination for webhook/slack sinks.
	URL string `yaml:"url"`
	// Events filters which reports are sent: "success", "failure", "update",
	// "error". Empty means all.
	Events []string `yaml:"events"`
	// Headers are extra HTTP headers (e.g. authorization) for the sink.
	Headers map[string]string `yaml:"headers"`
}

// RegistryConfig controls how drydock talks to image registries.
type RegistryConfig struct {
	// Auths maps a registry host (e.g. "ghcr.io") to credentials.
	Auths map[string]Auth `yaml:"auths"`
	// Retries is the number of retries for transient registry failures.
	Retries int `yaml:"retries"`
	// Timeout is the per-request HTTP timeout.
	Timeout Duration `yaml:"timeout"`
	// InsecureSkipVerify disables TLS verification. Only use for registries
	// with self-signed certificates on trusted networks.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
	// PlainHTTP talks to registries over HTTP (local test registries only).
	PlainHTTP bool `yaml:"plain_http"`
}

// UIConfig controls the embedded web dashboard.
type UIConfig struct {
	// Enabled serves the dashboard at "/". Requires API.Enabled.
	Enabled bool `yaml:"enabled"`
	// Username for the dashboard login. Defaults to "admin".
	Username string `yaml:"username"`
	// Password enables mutations in the dashboard. When empty the dashboard
	// is read-only: it can display state but never update or roll back.
	Password string `yaml:"password"`
}

// AuthEnabled reports whether dashboard actions are protected by a login.
func (u UIConfig) AuthEnabled() bool { return u.Password != "" }

// APIConfig controls the optional HTTP API and metrics endpoint.
type APIConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
	// Token, when set, requires "Authorization: Bearer <token>" on mutating
	// endpoints. Read-only endpoints and metrics remain open.
	Token string   `yaml:"token"`
	UI    UIConfig `yaml:"ui"`
}

// Override replaces policy values for a specific container by name. Values set
// here beat labels, which beat global defaults.
type Override struct {
	Enabled       *bool    `yaml:"enabled"`
	MonitorOnly   *bool    `yaml:"monitor_only"`
	NoPull        *bool    `yaml:"no_pull"`
	Schedule      string   `yaml:"schedule"`
	StopTimeout   Duration `yaml:"stop_timeout"`
	HealthTimeout Duration `yaml:"health_timeout"`
	Approve       *bool    `yaml:"approve"`
	Rollback      *bool    `yaml:"rollback"`
	Priority      *int     `yaml:"priority"`
}

// Config is drydock's complete runtime configuration.
type Config struct {
	// Interval between update runs when no cron Schedule is set.
	Interval Duration `yaml:"interval"`
	// Schedule is a cron expression (5-field, or 6-field with seconds).
	Schedule string `yaml:"schedule"`
	// Scope isolates multiple drydock instances: a container is only managed
	// by the instance whose scope matches its drydock.scope label.
	Scope string `yaml:"scope"`
	// IncludeStopped also considers stopped containers.
	IncludeStopped bool `yaml:"include_stopped"`
	// MonitorOnly reports updates without applying them globally.
	MonitorOnly bool `yaml:"monitor_only"`
	// LabelEnable lets containers opt in/out with the enable label.
	LabelEnable bool `yaml:"label_enable"`
	// LabelPrefix is the label namespace, e.g. "drydock".
	LabelPrefix string `yaml:"label_prefix"`
	// StopTimeout is the default graceful stop timeout.
	StopTimeout Duration `yaml:"stop_timeout"`
	// Concurrency is the maximum number of containers updated at once.
	Concurrency int `yaml:"concurrency"`
	// Cleanup removes the previous image after a successful update.
	Cleanup bool `yaml:"cleanup"`
	// KeepImages is how many previous image versions to retain for rollback
	// when Cleanup is enabled (0 removes immediately).
	KeepImages int `yaml:"keep_images"`
	// RemoveVolumes removes anonymous volumes with the replaced container.
	RemoveVolumes bool `yaml:"remove_volumes"`
	// HealthTimeout bounds how long drydock waits for a new container to
	// become healthy before rolling back.
	HealthTimeout Duration `yaml:"health_timeout"`
	// Settle is how long a container without a healthcheck must stay running
	// before the update is considered successful.
	Settle Duration `yaml:"settle"`
	// Rollback automatically restores the previous container when an update
	// fails its health check or exits immediately.
	Rollback bool `yaml:"rollback"`
	// DryRun reports what would change without pulling or recreating.
	DryRun bool `yaml:"dry_run"`
	// LogLevel is debug, info, warn, or error.
	LogLevel string `yaml:"log_level"`
	// LogFormat is "text" or "json".
	LogFormat string `yaml:"log_format"`
	// StateFile persists update history and rollback snapshots.
	StateFile string `yaml:"state_file"`
	// DockerHost overrides DOCKER_HOST.
	DockerHost string `yaml:"docker_host"`

	Registry      RegistryConfig      `yaml:"registry"`
	API           APIConfig           `yaml:"api"`
	Notifications []Notification      `yaml:"notifications"`
	Containers    map[string]Override `yaml:"containers"`
}

// Default returns a Config populated with safe, sensible defaults.
func Default() *Config {
	return &Config{
		Interval:      Duration{6 * time.Hour},
		LabelEnable:   false,
		LabelPrefix:   "drydock",
		StopTimeout:   Duration{30 * time.Second},
		Concurrency:   2,
		KeepImages:    2,
		HealthTimeout: Duration{2 * time.Minute},
		Settle:        Duration{3 * time.Second},
		Rollback:      true,
		LogLevel:      "info",
		LogFormat:     "text",
		StateFile:     defaultStateFile(),
		Registry: RegistryConfig{
			Retries: 3,
			Timeout: Duration{30 * time.Second},
		},
		API:        APIConfig{Enabled: false, Addr: ":8080", UI: UIConfig{Enabled: true}},
		Containers: map[string]Override{},
	}
}

// defaultStateFile prefers a per-user state directory so drydock works when
// run from a workstation as well as from a container.
func defaultStateFile() string {
	if v := os.Getenv("DRYDOCK_STATE_FILE"); v != "" {
		return v
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "drydock", "state.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "drydock", "state.json")
	}
	// Last resort for environments without a home directory. The container
	// image sets DRYDOCK_STATE_FILE=/var/lib/drydock/state.json explicitly.
	return "./drydock-state.json"
}

// Load reads a YAML config file into Default(). An empty path returns the
// defaults unchanged.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(expandEnv(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return cfg, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes ${VAR} references using the process environment.
// Only the braced form is expanded, so literal "$" characters in passwords
// are left alone unless they happen to look exactly like ${NAME}.
func expandEnv(data []byte) string {
	return envRef.ReplaceAllStringFunc(string(data), func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// Discover returns the first existing default config path, or "".
func Discover() string {
	candidates := []string{
		os.Getenv("DRYDOCK_CONFIG"),
		"drydock.yaml",
		"drydock.yml",
		"/etc/drydock/config.yaml",
		"/etc/drydock/config.yml",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// ApplyEnv layers DRYDOCK_* environment variables over the config.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) error {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	str := func(key string, dst *string) {
		if v, ok := lookup(key); ok && v != "" {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) error {
		v, ok := lookup(key)
		if !ok || v == "" {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = b
		return nil
	}
	integer := func(key string, dst *int) error {
		v, ok := lookup(key)
		if !ok || v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = n
		return nil
	}
	duration := func(key string, dst *Duration) error {
		v, ok := lookup(key)
		if !ok || v == "" {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		dst.Duration = d
		return nil
	}

	str("DRYDOCK_SCHEDULE", &c.Schedule)
	str("DRYDOCK_SCOPE", &c.Scope)
	str("DRYDOCK_LABEL_PREFIX", &c.LabelPrefix)
	str("DRYDOCK_LOG_LEVEL", &c.LogLevel)
	str("DRYDOCK_LOG_FORMAT", &c.LogFormat)
	str("DRYDOCK_STATE_FILE", &c.StateFile)
	str("DRYDOCK_DOCKER_HOST", &c.DockerHost)
	str("DRYDOCK_API_ADDR", &c.API.Addr)
	str("DRYDOCK_API_TOKEN", &c.API.Token)
	str("DRYDOCK_UI_USERNAME", &c.API.UI.Username)
	str("DRYDOCK_UI_PASSWORD", &c.API.UI.Password)

	for _, step := range []error{
		duration("DRYDOCK_INTERVAL", &c.Interval),
		duration("DRYDOCK_STOP_TIMEOUT", &c.StopTimeout),
		duration("DRYDOCK_HEALTH_TIMEOUT", &c.HealthTimeout),
		duration("DRYDOCK_SETTLE", &c.Settle),
		boolean("DRYDOCK_INCLUDE_STOPPED", &c.IncludeStopped),
		boolean("DRYDOCK_MONITOR_ONLY", &c.MonitorOnly),
		boolean("DRYDOCK_LABEL_ENABLE", &c.LabelEnable),
		boolean("DRYDOCK_CLEANUP", &c.Cleanup),
		boolean("DRYDOCK_REMOVE_VOLUMES", &c.RemoveVolumes),
		boolean("DRYDOCK_ROLLBACK", &c.Rollback),
		boolean("DRYDOCK_DRY_RUN", &c.DryRun),
		boolean("DRYDOCK_API_ENABLED", &c.API.Enabled),
		boolean("DRYDOCK_UI_ENABLED", &c.API.UI.Enabled),
		boolean("DRYDOCK_REGISTRY_INSECURE_SKIP_VERIFY", &c.Registry.InsecureSkipVerify),
		integer("DRYDOCK_CONCURRENCY", &c.Concurrency),
		integer("DRYDOCK_KEEP_IMAGES", &c.KeepImages),
	} {
		if step != nil {
			return step
		}
	}

	if c.DockerHost == "" {
		if v, ok := lookup("DOCKER_HOST"); ok {
			c.DockerHost = v
		}
	}
	return nil
}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	if c.Interval.Duration <= 0 && c.Schedule == "" {
		return fmt.Errorf("interval must be positive")
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1")
	}
	if c.KeepImages < 0 {
		return fmt.Errorf("keep_images must not be negative")
	}
	if strings.TrimSpace(c.LabelPrefix) == "" {
		return fmt.Errorf("label_prefix must not be empty")
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		return fmt.Errorf("log_format must be text or json, got %q", c.LogFormat)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn or error, got %q", c.LogLevel)
	}
	for i, n := range c.Notifications {
		switch n.Type {
		case "webhook", "slack", "json":
		default:
			return fmt.Errorf("notifications[%d]: unknown type %q", i, n.Type)
		}
		if n.URL == "" {
			return fmt.Errorf("notifications[%d]: url is required", i)
		}
	}
	return nil
}
