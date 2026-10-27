// Package policy turns global configuration plus per-container labels into an
// actionable plan: which containers drydock manages, how it updates them, and
// in what order.
package policy

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/model"
)

// Well-known label suffixes. The full key is "<prefix>.<suffix>", for example
// "drydock.monitor-only".
const (
	LabelEnable        = "enable"
	LabelMonitorOnly   = "monitor-only"
	LabelNoPull        = "no-pull"
	LabelApprove       = "approve"
	LabelRollback      = "rollback"
	LabelCleanup       = "cleanup"
	LabelSchedule      = "schedule"
	LabelStopTimeout   = "stop-timeout"
	LabelHealthTimeout = "health-timeout"
	LabelSettle        = "settle"
	LabelPriority      = "priority"
	LabelDependsOn     = "depends-on"
	LabelScope         = "scope"
	LabelSelf          = "self"
	LabelHealthPort    = "health-port"
	LabelHealthURL     = "health-url"
)

const (
	composeDependsOn = "com.docker.compose.depends_on"
	composeProject   = "com.docker.compose.project"
	composeService   = "com.docker.compose.service"
)

// Policy is the resolved update behaviour for a single container.
type Policy struct {
	Container     *model.Container
	Enabled       bool
	MonitorOnly   bool
	NoPull        bool
	Approve       bool
	Rollback      bool
	Cleanup       bool
	Self          bool
	Schedule      string
	StopTimeout   time.Duration
	HealthTimeout time.Duration
	Settle        time.Duration
	Priority      int
	DependsOn     []string
	HealthPort    int
	HealthURL     string
}

// Reason explains why a container was excluded from management.
type Reason string

const (
	ReasonDisabled   Reason = "disabled by label"
	ReasonOptIn      Reason = "not opted in (label_enable=true)"
	ReasonScope      Reason = "scope mismatch"
	ReasonSelf       Reason = "drydock's own container"
	ReasonInvalid    Reason = "invalid policy label"
	ReasonNotRunning Reason = "not running"
)

// Skipped describes a container that policy excluded, with a human reason.
type Skipped struct {
	Container *model.Container
	Reason    Reason
	Err       error
}

func (s Skipped) String() string {
	if s.Err != nil {
		return fmt.Sprintf("%s: %v", s.Reason, s.Err)
	}
	return string(s.Reason)
}

// Resolve evaluates all labels for one container. The returned error is
// non-nil when a label value is malformed; the policy is still usable with
// defaults for the bad field.
func Resolve(c *model.Container, cfg *config.Config) (Policy, error) {
	prefix := cfg.LabelPrefix
	get := func(suffix string) (string, bool) {
		v, ok := c.Labels()[prefix+"."+suffix]
		return v, ok
	}

	p := Policy{
		Container:     c,
		Enabled:       true,
		MonitorOnly:   cfg.MonitorOnly,
		Rollback:      cfg.Rollback,
		Cleanup:       cfg.Cleanup,
		StopTimeout:   cfg.StopTimeout.Duration,
		HealthTimeout: cfg.HealthTimeout.Duration,
		Settle:        cfg.Settle.Duration,
		Schedule:      cfg.Schedule,
	}

	var errs []error
	bad := func(suffix, val string, err error) {
		errs = append(errs, fmt.Errorf("label %s.%s=%q: %w", prefix, suffix, val, err))
	}

	if v, ok := get(LabelEnable); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			bad(LabelEnable, v, err)
		} else {
			p.Enabled = b
		}
	} else if cfg.LabelEnable {
		// Opt-in mode: only containers that explicitly enable themselves are
		// managed. An explicit "false" always wins.
		p.Enabled = false
	}
	if v, ok := get(LabelMonitorOnly); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(LabelMonitorOnly, v, err)
		} else {
			p.MonitorOnly = b
		}
	}
	if v, ok := get(LabelNoPull); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(LabelNoPull, v, err)
		} else {
			p.NoPull = b
		}
	}
	if v, ok := get(LabelApprove); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(LabelApprove, v, err)
		} else {
			p.Approve = b
		}
	}
	if v, ok := get(LabelRollback); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(LabelRollback, v, err)
		} else {
			p.Rollback = b
		}
	}
	if v, ok := get(LabelCleanup); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(LabelCleanup, v, err)
		} else {
			p.Cleanup = b
		}
	}
	if v, ok := get(LabelSelf); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			p.Self = b
		}
	}
	if v, ok := get(LabelSchedule); ok && strings.TrimSpace(v) != "" {
		p.Schedule = strings.TrimSpace(v)
	}
	if v, ok := get(LabelStopTimeout); ok {
		if d, err := parseSecondsOrDuration(v); err != nil {
			bad(LabelStopTimeout, v, err)
		} else {
			p.StopTimeout = d
		}
	}
	if v, ok := get(LabelHealthTimeout); ok {
		if d, err := parseSecondsOrDuration(v); err != nil {
			bad(LabelHealthTimeout, v, err)
		} else {
			p.HealthTimeout = d
		}
	}
	if v, ok := get(LabelSettle); ok {
		if d, err := parseSecondsOrDuration(v); err != nil {
			bad(LabelSettle, v, err)
		} else {
			p.Settle = d
		}
	}
	if v, ok := get(LabelPriority); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil {
			bad(LabelPriority, v, err)
		} else {
			p.Priority = n
		}
	}
	if v, ok := get(LabelHealthPort); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n <= 0 || n > 65535 {
			if err == nil {
				err = fmt.Errorf("port out of range")
			}
			bad(LabelHealthPort, v, err)
		} else {
			p.HealthPort = n
		}
	}
	if v, ok := get(LabelHealthURL); ok {
		p.HealthURL = strings.TrimSpace(v)
	}
	if v, ok := get(LabelDependsOn); ok {
		p.DependsOn = append(p.DependsOn, splitList(v)...)
	}
	p.DependsOn = append(p.DependsOn, composeDependencies(c)...)

	// A per-container override in the config file wins over labels.
	if ov, ok := cfg.Containers[c.Name()]; ok {
		applyOverride(&p, ov)
	}

	if len(errs) > 0 {
		return p, joinErrs(errs)
	}
	return p, nil
}

func applyOverride(p *Policy, ov config.Override) {
	if ov.Enabled != nil {
		p.Enabled = *ov.Enabled
	}
	if ov.MonitorOnly != nil {
		p.MonitorOnly = *ov.MonitorOnly
	}
	if ov.NoPull != nil {
		p.NoPull = *ov.NoPull
	}
	if ov.Approve != nil {
		p.Approve = *ov.Approve
	}
	if ov.Rollback != nil {
		p.Rollback = *ov.Rollback
	}
	if ov.Schedule != "" {
		p.Schedule = ov.Schedule
	}
	if ov.StopTimeout.Duration > 0 {
		p.StopTimeout = ov.StopTimeout.Duration
	}
	if ov.HealthTimeout.Duration > 0 {
		p.HealthTimeout = ov.HealthTimeout.Duration
	}
	if ov.Priority != nil {
		p.Priority = *ov.Priority
	}
}

// Select resolves policy for every container and splits the result into
// managed policies and skipped containers with reasons.
func Select(containers []*model.Container, cfg *config.Config) ([]Policy, []Skipped) {
	policies := make([]Policy, 0, len(containers))
	var skipped []Skipped

	for _, c := range containers {
		p, err := Resolve(c, cfg)
		if err != nil {
			skipped = append(skipped, Skipped{Container: c, Reason: ReasonInvalid, Err: err})
			continue
		}
		if !p.Enabled {
			reason := ReasonDisabled
			if cfg.LabelEnable {
				reason = ReasonOptIn
			}
			skipped = append(skipped, Skipped{Container: c, Reason: reason})
			continue
		}
		if !scopeMatches(c, cfg) {
			skipped = append(skipped, Skipped{Container: c, Reason: ReasonScope})
			continue
		}
		if p.Self || isSelfContainer(c) {
			skipped = append(skipped, Skipped{Container: c, Reason: ReasonSelf})
			continue
		}
		if !c.Running() && !cfg.IncludeStopped {
			skipped = append(skipped, Skipped{Container: c, Reason: ReasonNotRunning})
			continue
		}
		policies = append(policies, p)
	}
	return policies, skipped
}

func scopeMatches(c *model.Container, cfg *config.Config) bool {
	got := c.Labels()[cfg.LabelPrefix+"."+LabelScope]
	return got == cfg.Scope
}

// isSelfContainer detects drydock's own container without requiring a label.
// Docker sets a container's hostname to its short ID unless it was overridden,
// so comparing the two is reliable for the normal Compose deployment.
func isSelfContainer(c *model.Container) bool {
	host, err := os.Hostname()
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	id := c.ID()
	return id == host || (len(host) >= 12 && strings.HasPrefix(id, host))
}

// Level is a set of policies that can be updated concurrently because none of
// them depend on one another.
type Level []Policy

// Plan orders policies for a safe update: dependencies first (so a database is
// upgraded before the app that uses it), higher priority first within a level.
func Plan(policies []Policy) ([]Level, error) {
	if len(policies) == 0 {
		return nil, nil
	}

	byName := make(map[string]Policy, len(policies))
	serviceIndex := make(map[string][]string, len(policies))
	order := make([]string, 0, len(policies))
	for _, p := range policies {
		name := p.Container.Name()
		byName[name] = p
		order = append(order, name)
		if svc := p.Container.Labels()[composeService]; svc != "" {
			key := p.Container.Labels()[composeProject] + "/" + svc
			serviceIndex[key] = append(serviceIndex[key], name)
		}
	}

	deps := make(map[string][]string, len(policies))
	indegree := make(map[string]int, len(policies))
	for _, name := range order {
		indegree[name] = 0
	}
	for _, p := range policies {
		name := p.Container.Name()
		for _, dep := range p.DependsOn {
			targets := resolveDependency(dep, p.Container, byName, serviceIndex)
			for _, t := range targets {
				if t == name {
					continue
				}
				deps[t] = append(deps[t], name)
				indegree[name]++
			}
		}
	}

	var levels []Level
	remaining := len(order)
	placed := make(map[string]bool, len(order))
	for remaining > 0 {
		var ready []string
		for _, name := range order {
			if !placed[name] && indegree[name] == 0 {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			var stuck []string
			for _, name := range order {
				if !placed[name] {
					stuck = append(stuck, name)
				}
			}
			sort.Strings(stuck)
			return nil, fmt.Errorf("dependency cycle detected between: %s", strings.Join(stuck, ", "))
		}
		sort.Slice(ready, func(i, j int) bool {
			pi, pj := byName[ready[i]].Priority, byName[ready[j]].Priority
			if pi != pj {
				return pi > pj
			}
			return ready[i] < ready[j]
		})
		level := make(Level, 0, len(ready))
		for _, name := range ready {
			level = append(level, byName[name])
			placed[name] = true
			remaining--
		}
		levels = append(levels, level)
		for _, name := range ready {
			for _, dependent := range deps[name] {
				indegree[dependent]--
			}
		}
	}
	return levels, nil
}

func resolveDependency(dep string, self *model.Container, byName map[string]Policy, serviceIndex map[string][]string) []string {
	dep = strings.TrimSpace(dep)
	if dep == "" {
		return nil
	}
	// Exact container name.
	if _, ok := byName[dep]; ok {
		return []string{dep}
	}
	// Compose service name within the same project.
	project := self.Labels()[composeProject]
	if project != "" {
		if names, ok := serviceIndex[project+"/"+dep]; ok {
			return names
		}
	}
	// Anything sharing the suffix (compose v1 naming: project_service_1).
	var matches []string
	for name := range byName {
		if name == dep || strings.HasSuffix(name, "-"+dep+"-1") || strings.HasSuffix(name, "_"+dep+"_1") {
			matches = append(matches, name)
		}
	}
	return matches
}

func composeDependencies(c *model.Container) []string {
	raw := c.Labels()[composeDependsOn]
	if raw == "" {
		return nil
	}
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Format: "<service>:<condition>:<required>"
		svc, _, _ := strings.Cut(entry, ":")
		if svc = strings.TrimSpace(svc); svc != "" {
			out = append(out, svc)
		}
	}
	return out
}

// ByName finds a policy for a container name.
func ByName(policies []Policy, name string) (Policy, bool) {
	for _, p := range policies {
		if p.Container.Name() == name {
			return p, true
		}
	}
	return Policy{}, false
}

// Names returns the container names in a level.
func (l Level) Names() []string {
	out := make([]string, len(l))
	for i, p := range l {
		out[i] = p.Container.Name()
	}
	return out
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseSecondsOrDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("must not be negative")
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return d, nil
}

func joinErrs(errs []error) error {
	var msgs []string
	for _, e := range errs {
		if e != nil {
			msgs = append(msgs, e.Error())
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
