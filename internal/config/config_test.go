package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestLoadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drydock.yaml")
	yaml := `
interval: 90m
concurrency: 4
rollback: false
keep_images: 5
state_file: /tmp/drydock-state.json
registry:
  retries: 7
  timeout: 10s
  auths:
    ghcr.io:
      username: bot
      password: secret
notifications:
  - type: webhook
    url: https://example.invalid/hook
    events: [failure]
containers:
  web:
    monitor_only: true
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval.Duration != 90*time.Minute {
		t.Errorf("interval = %s", cfg.Interval.Duration)
	}
	if cfg.Concurrency != 4 {
		t.Errorf("concurrency = %d", cfg.Concurrency)
	}
	if cfg.Rollback {
		t.Error("rollback should be false")
	}
	if cfg.KeepImages != 5 {
		t.Errorf("keep_images = %d", cfg.KeepImages)
	}
	if cfg.Registry.Retries != 7 || cfg.Registry.Timeout.Duration != 10*time.Second {
		t.Errorf("registry = %+v", cfg.Registry)
	}
	if cfg.Registry.Auths["ghcr.io"].Username != "bot" {
		t.Errorf("auth = %+v", cfg.Registry.Auths)
	}
	if len(cfg.Notifications) != 1 || cfg.Notifications[0].Type != "webhook" {
		t.Errorf("notifications = %+v", cfg.Notifications)
	}
	if ov, ok := cfg.Containers["web"]; !ok || ov.MonitorOnly == nil || !*ov.MonitorOnly {
		t.Errorf("container override = %+v", cfg.Containers)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("intervall: 5m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drydock.yaml")
	if err := os.WriteFile(path, []byte("concurrency: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"DRYDOCK_CONCURRENCY":  "9",
		"DRYDOCK_INTERVAL":     "15m",
		"DRYDOCK_ROLLBACK":     "false",
		"DRYDOCK_API_ADDR":     ":9999",
		"DRYDOCK_LABEL_PREFIX": "updater",
		"DOCKER_HOST":          "unix:///run/docker.sock",
	}
	err = cfg.ApplyEnv(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency != 9 {
		t.Errorf("concurrency = %d", cfg.Concurrency)
	}
	if cfg.Interval.Duration != 15*time.Minute {
		t.Errorf("interval = %s", cfg.Interval.Duration)
	}
	if cfg.Rollback {
		t.Error("rollback should be false")
	}
	if cfg.API.Addr != ":9999" || cfg.LabelPrefix != "updater" {
		t.Errorf("api addr = %q, prefix = %q", cfg.API.Addr, cfg.LabelPrefix)
	}
	if cfg.DockerHost != "unix:///run/docker.sock" {
		t.Errorf("docker host = %q", cfg.DockerHost)
	}
}

func TestApplyEnvRejectsBadValues(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyEnv(func(k string) (string, bool) {
		if k == "DRYDOCK_CONCURRENCY" {
			return "many", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDefaultStateFileIsUserWritable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DRYDOCK_STATE_FILE", "")
	t.Setenv("XDG_STATE_HOME", dir)
	cfg := Default()
	want := filepath.Join(dir, "drydock", "state.json")
	if cfg.StateFile != want {
		t.Errorf("state file = %q, want %q", cfg.StateFile, want)
	}
	if strings.HasPrefix(cfg.StateFile, "/var/lib/") {
		t.Errorf("default state file must not require root: %q", cfg.StateFile)
	}
}

func TestValidate(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}

	bad := []*Config{
		{Interval: Duration{}, LogLevel: "info", LogFormat: "text", LabelPrefix: "x", Concurrency: 1},
		{Interval: Duration{time.Hour}, LogLevel: "nope", LogFormat: "text", LabelPrefix: "x", Concurrency: 1},
		{Interval: Duration{time.Hour}, LogLevel: "info", LogFormat: "xml", LabelPrefix: "x", Concurrency: 1},
		{Interval: Duration{time.Hour}, LogLevel: "info", LogFormat: "text", LabelPrefix: "", Concurrency: 1},
		{Interval: Duration{time.Hour}, LogLevel: "info", LogFormat: "text", LabelPrefix: "x", Concurrency: 0},
		{Interval: Duration{time.Hour}, LogLevel: "info", LogFormat: "text", LabelPrefix: "x", Concurrency: 1, Notifications: []Notification{{Type: "pager", URL: "x"}}},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected a validation error", i)
		}
	}
}

func TestDurationUnmarshalIntegerSeconds(t *testing.T) {
	var holder struct {
		D Duration `yaml:"d"`
	}
	if err := yaml.Unmarshal([]byte("d: 30\n"), &holder); err != nil {
		t.Fatal(err)
	}
	if holder.D.Duration != 30*time.Second {
		t.Errorf("duration = %s", holder.D.Duration)
	}
}
