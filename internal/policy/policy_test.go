package policy

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/model"
)

func ctr(name string, labels map[string]string, running bool) *model.Container {
	return &model.Container{
		Inspect: container.InspectResponse{
			ID:     name + "-id",
			Name:   "/" + name,
			Config: &container.Config{Image: name + ":latest", Labels: labels},
			State:  &container.State{Running: running, Status: container.StateRunning},
		},
	}
}

func TestResolveReadsLabels(t *testing.T) {
	cfg := config.Default()
	c := ctr("web", map[string]string{
		"drydock.monitor-only":   "true",
		"drydock.schedule":       "0 4 * * *",
		"drydock.stop-timeout":   "45",
		"drydock.health-timeout": "90s",
		"drydock.priority":       "7",
		"drydock.cleanup":        "true",
		"drydock.depends-on":     "db, cache",
	}, true)

	p, err := Resolve(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MonitorOnly {
		t.Error("monitor-only not applied")
	}
	if p.Schedule != "0 4 * * *" {
		t.Errorf("schedule = %q", p.Schedule)
	}
	if p.StopTimeout != 45*time.Second {
		t.Errorf("stop timeout = %s", p.StopTimeout)
	}
	if p.HealthTimeout != 90*time.Second {
		t.Errorf("health timeout = %s", p.HealthTimeout)
	}
	if p.Priority != 7 {
		t.Errorf("priority = %d", p.Priority)
	}
	if !p.Cleanup {
		t.Error("cleanup not applied")
	}
	if len(p.DependsOn) != 2 || p.DependsOn[0] != "db" || p.DependsOn[1] != "cache" {
		t.Errorf("depends-on = %v", p.DependsOn)
	}
}

func TestResolveRejectsBadLabel(t *testing.T) {
	cfg := config.Default()
	c := ctr("web", map[string]string{"drydock.priority": "high"}, true)
	if _, err := Resolve(c, cfg); err == nil {
		t.Fatal("expected an error for a non-numeric priority")
	}
}

func TestSelectHonoursOptInAndScope(t *testing.T) {
	cfg := config.Default()
	cfg.LabelEnable = true
	cfg.Scope = "prod"

	enabled := ctr("enabled", map[string]string{"drydock.enable": "true", "drydock.scope": "prod"}, true)
	disabled := ctr("disabled", map[string]string{"drydock.scope": "prod"}, true)
	wrongScope := ctr("staging", map[string]string{"drydock.enable": "true", "drydock.scope": "staging"}, true)
	stopped := ctr("stopped", map[string]string{"drydock.enable": "true", "drydock.scope": "prod"}, false)

	policies, skipped := Select([]*model.Container{enabled, disabled, wrongScope, stopped}, cfg)
	if len(policies) != 1 || policies[0].Container.Name() != "enabled" {
		t.Fatalf("selected %v", policyNames(policies))
	}
	if len(skipped) != 3 {
		t.Fatalf("expected 3 skipped, got %d", len(skipped))
	}

	// Stopped containers can be included explicitly.
	cfg.IncludeStopped = true
	policies, _ = Select([]*model.Container{stopped}, cfg)
	if len(policies) != 1 {
		t.Fatal("stopped container should be included when configured")
	}
}

func TestSelectSkipsDrydockItself(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("hostname unavailable")
	}
	self := ctr("drydock", nil, true)
	self.Inspect.ID = host

	policies, skipped := Select([]*model.Container{self}, config.Default())
	if len(policies) != 0 {
		t.Fatalf("drydock selected itself: %v", policyNames(policies))
	}
	if len(skipped) != 1 || skipped[0].Reason != ReasonSelf {
		t.Fatalf("skipped = %+v", skipped)
	}

	other := ctr("web", nil, true)
	policies, _ = Select([]*model.Container{other}, config.Default())
	if len(policies) != 1 {
		t.Fatalf("unrelated container was skipped: %v", skipped)
	}
}

func TestPlanRespectsDependencies(t *testing.T) {
	db := ctr("db", nil, true)
	web := ctr("web", map[string]string{"drydock.depends-on": "db"}, true)
	cache := ctr("cache", nil, true)

	policies, _ := Select([]*model.Container{web, db, cache}, config.Default())
	levels, err := Plan(policies)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d: %v", len(levels), levels)
	}
	if !contains(levels[0].Names(), "db") {
		t.Errorf("db must be updated first, got %v", levels[0].Names())
	}
	if !contains(levels[1].Names(), "web") {
		t.Errorf("web must wait for db, got %v", levels[1].Names())
	}
}

func TestPlanDetectsCycles(t *testing.T) {
	a := ctr("a", map[string]string{"drydock.depends-on": "b"}, true)
	b := ctr("b", map[string]string{"drydock.depends-on": "a"}, true)
	policies, _ := Select([]*model.Container{a, b}, config.Default())
	_, err := Plan(policies)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
}

func TestComposeDependsOnResolvesServices(t *testing.T) {
	db := ctr("proj-db-1", map[string]string{
		"com.docker.compose.project": "proj",
		"com.docker.compose.service": "db",
	}, true)
	web := ctr("proj-web-1", map[string]string{
		"com.docker.compose.project":    "proj",
		"com.docker.compose.service":    "web",
		"com.docker.compose.depends_on": "db:service_started:false",
	}, true)

	policies, _ := Select([]*model.Container{web, db}, config.Default())
	levels, err := Plan(policies)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 || !contains(levels[0].Names(), "proj-db-1") {
		t.Fatalf("compose dependency not honoured: %v", levels)
	}
}

func TestConfigOverrideBeatsLabel(t *testing.T) {
	cfg := config.Default()
	enabled := false
	cfg.Containers = map[string]config.Override{"web": {MonitorOnly: &enabled, Priority: ptr(3)}}
	p, err := Resolve(ctr("web", map[string]string{"drydock.monitor-only": "true"}, true), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.MonitorOnly {
		t.Error("config override should have disabled monitor-only")
	}
	if p.Priority != 3 {
		t.Errorf("priority = %d", p.Priority)
	}
}

func ptr[T any](v T) *T { return &v }

func policyNames(ps []Policy) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Container.Name()
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
