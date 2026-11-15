// Package engine orchestrates a full update cycle: discover, plan, pull,
// recreate, verify, roll back, record, and notify.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/docker"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/model"
	"github.com/sasho/drydock/internal/notify"
	"github.com/sasho/drydock/internal/policy"
	"github.com/sasho/drydock/internal/registry"
	"github.com/sasho/drydock/internal/state"
)

// Docker is the subset of the Docker runtime the engine needs. It is an
// interface so update logic can be tested without a daemon.
type Docker interface {
	Ping(ctx context.Context) error
	List(ctx context.Context) ([]*model.Container, error)
	Inspect(ctx context.Context, id string) (*model.Container, error)
	Pull(ctx context.Context, ref, auth string) (string, error)
	Recreate(ctx context.Context, c *model.Container, opts docker.RecreateOptions) (docker.RecreateResult, error)
	Rollback(ctx context.Context, current *model.Container, target model.Snapshot, health docker.HealthOptions) (string, error)
	RemoveImage(ctx context.Context, id string) error
	HasImage(ctx context.Context, id string) bool
}

// Resolver resolves remote image digests. Implemented by *registry.Client.
type Resolver interface {
	Resolve(ctx context.Context, ref string, p registry.Platform) (registry.Result, error)
	DockerAuth(ref string) string
}

// Options tune a single run.
type Options struct {
	// CheckOnly resolves digests but never pulls or recreates.
	CheckOnly bool
	// DryRun reports what would change without pulling or recreating.
	DryRun bool
	// Force applies updates that would otherwise require approval.
	Force bool
	// Only restricts the run to the named containers (name, prefix, or ID).
	Only []string
}

// Outcome is the result for one container in a run.
type Outcome struct {
	Container  string        `json:"container"`
	Image      string        `json:"image"`
	Status     state.Status  `json:"status"`
	Message    string        `json:"message,omitempty"`
	OldImageID string        `json:"old_image_id,omitempty"`
	NewImageID string        `json:"new_image_id,omitempty"`
	OldDigest  string        `json:"old_digest,omitempty"`
	NewDigest  string        `json:"new_digest,omitempty"`
	RolledBack bool          `json:"rolled_back,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
}

// Report summarises a run.
type Report struct {
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration"`
	DryRun    bool          `json:"dry_run"`
	Outcomes  []Outcome     `json:"outcomes"`
	Skipped   int           `json:"skipped"`
}

// Counts returns outcomes grouped by status.
func (r *Report) Counts() map[state.Status]int {
	out := map[state.Status]int{}
	for _, o := range r.Outcomes {
		out[o.Status]++
	}
	return out
}

// Engine runs update cycles.
type Engine struct {
	cfg      *config.Config
	docker   Docker
	resolver Resolver
	store    *state.Store
	notify   *notify.Dispatcher
	metrics  *metrics.Registry
	log      *slog.Logger

	// runMu serialises update cycles and rollbacks so the API and the
	// scheduler can never operate on the same container concurrently.
	runMu sync.Mutex

	// jobs tracks asynchronous runs started from the API and dashboard.
	jobsMu   sync.Mutex
	jobs     map[string]*Job
	jobOrder []string
}

// New wires an engine together.
func New(cfg *config.Config, d Docker, r Resolver, st *state.Store, n *notify.Dispatcher, m *metrics.Registry, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	if m == nil {
		m = metrics.New()
	}
	return &Engine{cfg: cfg, docker: d, resolver: r, store: st, notify: n, metrics: m, log: log}
}

// Run performs one complete cycle.
func (e *Engine) Run(ctx context.Context, opts Options) (*Report, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	started := time.Now()
	containers, err := e.docker.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	policies, skipped := policy.Select(containers, e.cfg)
	policies = filterOnly(policies, opts.Only)
	e.metrics.SetContainers(len(policies))
	e.metrics.SetPending(len(e.store.Pending()))

	e.log.Info("scan starting",
		"managed", len(policies),
		"skipped", len(skipped),
		"only", opts.Only,
		"dry_run", opts.DryRun || e.cfg.DryRun,
		"check_only", opts.CheckOnly,
	)
	for _, s := range skipped {
		e.log.Debug("container not managed", "container", s.Container.Name(), "reason", s.String())
	}

	levels, err := policy.Plan(policies)
	if err != nil {
		return nil, fmt.Errorf("plan updates: %w", err)
	}

	// Levels are sequential; containers inside a level run concurrently.
	var outcomes []Outcome
	for _, level := range levels {
		if err := ctx.Err(); err != nil {
			break
		}
		outcomes = append(outcomes, e.runLevel(ctx, level, opts)...)
	}

	report := &Report{
		StartedAt: started.UTC(),
		Duration:  time.Since(started),
		DryRun:    opts.DryRun || e.cfg.DryRun,
		Outcomes:  outcomes,
		Skipped:   len(skipped),
	}
	sort.SliceStable(report.Outcomes, func(i, j int) bool {
		return report.Outcomes[i].Container < report.Outcomes[j].Container
	})

	e.metrics.ObserveRun(report.Duration)
	e.metrics.SetPending(len(e.store.Pending()))
	e.notifyReport(ctx, report)
	return report, nil
}

func (e *Engine) runLevel(ctx context.Context, level policy.Level, opts Options) []Outcome {
	sem := make(chan struct{}, max(e.cfg.Concurrency, 1))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		outcomes []Outcome
	)
	for _, p := range level {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				outcomes = append(outcomes, Outcome{
					Container: p.Container.Name(),
					Image:     p.Container.ImageRef(),
					Status:    state.StatusFailed,
					Message:   ctx.Err().Error(),
				})
				mu.Unlock()
				return
			}
			o := e.process(ctx, p, opts)
			mu.Lock()
			outcomes = append(outcomes, o)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return outcomes
}

func (e *Engine) process(ctx context.Context, p policy.Policy, opts Options) (out Outcome) {
	start := time.Now()
	c := p.Container
	out = Outcome{Container: c.Name(), Image: c.ImageRef(), OldImageID: c.ImageID()}

	finish := func(status state.Status, msg string, snap *model.Snapshot) Outcome {
		out.Status = status
		out.Message = msg
		out.Duration = time.Since(start)
		level := slog.LevelInfo
		if status == state.StatusFailed || status == state.StatusRolledBack {
			level = slog.LevelWarn
		}
		e.log.Log(ctx, level, "container processed",
			"container", out.Container, "image", out.Image, "status", status, "message", msg,
			"duration", out.Duration.Round(time.Millisecond))
		e.metrics.IncUpdate(string(status))
		// History is an audit trail of changes, not of every scan. Keep it to
		// events an operator may need to investigate or reverse.
		if status == state.StatusUpdated || status == state.StatusFailed || status == state.StatusRolledBack {
			e.recordWithSnapshot(c, out, snap, status, msg)
		}
		return out
	}

	if c.Pinned() {
		e.metrics.IncScan("pinned")
		return finish(state.StatusUpToDate, "image is digest-pinned; it can never move", nil)
	}

	osName, arch, variant := c.Platform()
	remote, err := e.resolver.Resolve(ctx, c.ImageRef(), registry.Platform{OS: osName, Arch: arch, Variant: variant})
	if err != nil {
		e.metrics.IncScan("error")
		return finish(state.StatusFailed, "registry check: "+err.Error(), nil)
	}
	out.NewDigest = remote.Digest
	out.OldDigest = strings.Join(c.LocalDigests(), ",")
	out.NewImageID = ""

	if digestsMatch(c.LocalDigests(), remote) {
		e.metrics.IncScan("up_to_date")
		_ = e.store.TrackImage(c.Name(), c.ImageID())
		// The image already matches the registry, so any pending record for
		// this container is stale (for example after an external update).
		_ = e.store.ClearPending(c.Name())
		return finish(state.StatusUpToDate, "", nil)
	}
	e.metrics.IncScan("update_available")

	dry := opts.DryRun || e.cfg.DryRun
	switch {
	case opts.CheckOnly:
		_ = e.recordPending(c, remote)
		return finish(state.StatusPending, "update available (check only)", nil)
	case dry:
		_ = e.recordPending(c, remote)
		return finish(state.StatusPending, "update available (dry run)", nil)
	case p.MonitorOnly:
		_ = e.recordPending(c, remote)
		return finish(state.StatusPending, "update available (monitor only)", nil)
	case p.Approve && !opts.Force:
		_ = e.recordPending(c, remote)
		return finish(state.StatusPending, "update available (awaiting approval)", nil)
	}

	auth := e.resolver.DockerAuth(c.ImageRef())
	newID, err := e.docker.Pull(ctx, c.ImageRef(), auth)
	if err != nil {
		return finish(state.StatusFailed, "pull: "+err.Error(), nil)
	}
	if newID == c.ImageID() {
		_ = e.store.TrackImage(c.Name(), newID)
		_ = e.store.ClearPending(c.Name())
		return finish(state.StatusUpToDate, "tag moved but image content is unchanged", nil)
	}

	// Re-inspect immediately before replacing: the container may have been
	// restarted manually, and its current image ID drives the decision.
	if fresh, err := e.docker.Inspect(ctx, c.ID()); err == nil {
		c = fresh
	}

	health := docker.HealthOptions{
		Timeout: p.HealthTimeout,
		Settle:  p.Settle,
		Port:    p.HealthPort,
		URL:     p.HealthURL,
	}
	res, err := e.docker.Recreate(ctx, c, docker.RecreateOptions{
		ImageRef:      c.ImageRef(),
		ImageID:       newID,
		StopTimeout:   p.StopTimeout,
		Rollback:      p.Rollback,
		Health:        health,
		RemoveVolumes: e.cfg.RemoveVolumes,
	})
	if err != nil {
		status := state.StatusFailed
		if res.RolledBack {
			status = state.StatusRolledBack
			e.metrics.IncRollback()
		}
		out.RolledBack = res.RolledBack
		out.NewImageID = newID
		var snap *model.Snapshot
		if res.Snapshot.ContainerID != "" {
			s := res.Snapshot
			snap = &s
		}
		return finish(status, err.Error(), snap)
	}

	// Success: remember both image IDs for rollback and pruning.
	out.NewImageID = newID
	_ = e.store.TrackImage(c.Name(), newID)
	_ = e.store.TrackImage(c.Name(), res.Snapshot.ImageID)
	_ = e.store.ClearPending(c.Name())
	e.pruneImages(ctx, c.Name(), p.Cleanup)

	snap := res.Snapshot
	e.log.Info("container updated",
		"container", c.Name(), "image", c.ImageRef(),
		"old_image", short(res.Snapshot.ImageID), "new_image", short(newID))
	return finish(state.StatusUpdated, "", &snap)
}

func (e *Engine) recordWithSnapshot(c *model.Container, out Outcome, snap *model.Snapshot, status state.Status, msg string) {
	entry := state.Entry{
		Time:        time.Now().UTC(),
		Container:   c.Name(),
		ContainerID: c.ID(),
		Image:       c.ImageRef(),
		OldImageID:  out.OldImageID,
		NewImageID:  out.NewImageID,
		OldDigest:   out.OldDigest,
		NewDigest:   out.NewDigest,
		Status:      status,
		Message:     msg,
		RolledBack:  out.RolledBack,
		DurationMS:  out.Duration.Milliseconds(),
		Snapshot:    snap,
	}
	if err := e.store.Append(entry); err != nil {
		e.log.Error("could not persist history", "container", c.Name(), "error", err)
	}
}

func (e *Engine) recordPending(c *model.Container, remote registry.Result) error {
	return e.store.SetPending(state.Pending{
		Container:  c.Name(),
		Image:      c.ImageRef(),
		OldImageID: c.ImageID(),
		NewDigest:  remote.Digest,
	})
}

func (e *Engine) pruneImages(ctx context.Context, container string, cleanup bool) {
	if !cleanup {
		return
	}
	// Always retain the currently running image plus keep_images previous ones.
	keep := max(e.cfg.KeepImages, 0) + 1
	drop := e.store.ImagesToPrune(container, keep)
	for _, id := range drop {
		if err := e.docker.RemoveImage(ctx, id); err != nil {
			e.log.Warn("could not prune old image", "container", container, "image", short(id), "error", err)
			continue
		}
		e.store.ForgetImage(container, id)
		e.log.Debug("pruned old image", "container", container, "image", short(id))
	}
	if err := e.store.Save(); err != nil {
		e.log.Warn("could not save state after pruning", "error", err)
	}
}

// Rollback restores the container to the state before its last successful
// update.
func (e *Engine) Rollback(ctx context.Context, name string) (Outcome, error) {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	start := time.Now()
	out := Outcome{Container: name}

	entry, ok := e.store.LastSuccessful(name)
	if !ok || entry.Snapshot == nil {
		return out, fmt.Errorf("no rollback point found for %q", name)
	}

	containers, err := e.docker.List(ctx)
	if err != nil {
		return out, err
	}
	var current *model.Container
	for _, c := range containers {
		if c.Name() == name || c.ID() == name || strings.HasPrefix(c.ID(), name) {
			current = c
			break
		}
	}
	if current == nil {
		return out, fmt.Errorf("container %q not found", name)
	}
	out.Image = current.ImageRef()
	out.OldImageID = current.ImageID()
	out.NewImageID = entry.Snapshot.ImageID

	p, perr := policy.Resolve(current, e.cfg)
	if perr != nil {
		e.log.Warn("using default policy for rollback due to bad labels", "container", name, "error", perr)
	}
	preRollback := current.Snapshot()
	health := docker.HealthOptions{Timeout: p.HealthTimeout, Settle: p.Settle, Port: p.HealthPort, URL: p.HealthURL}
	if _, err := e.docker.Rollback(ctx, current, *entry.Snapshot, health); err != nil {
		out.Status = state.StatusFailed
		out.Message = err.Error()
		out.Duration = time.Since(start)
		e.recordWithSnapshot(current, out, nil, state.StatusFailed, out.Message)
		e.metrics.IncUpdate("failed")
		return out, err
	}

	out.Status = state.StatusRolledBack
	out.Message = "rolled back to image " + short(entry.Snapshot.ImageID)
	out.Duration = time.Since(start)
	// Keep the state we rolled back *from* so the operation can be inspected
	// (and, if desired, reversed) later.
	e.recordWithSnapshot(current, out, &preRollback, state.StatusRolledBack, out.Message)
	// The update that produced this pending entry is no longer applied, so
	// the stale approval must not linger on the dashboard.
	_ = e.store.ClearPending(current.Name())
	e.metrics.IncRollback()
	e.metrics.IncUpdate(string(state.StatusRolledBack))
	e.log.Info("container rolled back", "container", name, "image", short(entry.Snapshot.ImageID))
	return out, nil
}

// Ready reports whether the Docker daemon is reachable.
func (e *Engine) Ready(ctx context.Context) error { return e.docker.Ping(ctx) }

// Managed returns the containers drydock would manage, without network calls.
func (e *Engine) Managed(ctx context.Context) ([]policy.Policy, int, error) {
	containers, err := e.docker.List(ctx)
	if err != nil {
		return nil, 0, err
	}
	policies, skipped := policy.Select(containers, e.cfg)
	return policies, len(skipped), nil
}

// Containers lists containers with their resolved policy, without performing
// any registry requests. Used by the API.
func (e *Engine) Containers(ctx context.Context) ([]ContainerView, error) {
	containers, err := e.docker.List(ctx)
	if err != nil {
		return nil, err
	}
	policies, _ := policy.Select(containers, e.cfg)
	byName := make(map[string]policy.Policy, len(policies))
	for _, p := range policies {
		byName[p.Container.Name()] = p
	}
	views := make([]ContainerView, 0, len(containers))
	for _, c := range containers {
		v := ContainerView{
			Name:         c.Name(),
			ID:           c.ID(),
			Image:        c.ImageRef(),
			ImageID:      c.ImageID(),
			Digests:      c.LocalDigests(),
			Labels:       c.Labels(),
			Networks:     networkNames(c),
			Status:       c.Status(),
			Health:       c.Health(),
			Running:      c.Running(),
			RestartCount: c.RestartCount(),
			Created:      c.Created(),
			Pinned:       c.Pinned(),
			AutoRemove:   c.AutoRemove(),
			StopSignal:   c.StopSignal(),
		}
		if p, ok := byName[c.Name()]; ok {
			v.Managed = true
			v.MonitorOnly = p.MonitorOnly
			v.Approve = p.Approve
			v.Rollback = p.Rollback
			v.Cleanup = p.Cleanup
			v.Schedule = p.Schedule
			v.Priority = p.Priority
			v.DependsOn = p.DependsOn
			v.HealthPort = p.HealthPort
			v.HealthURL = p.HealthURL
			v.StopTimeout = int(p.StopTimeout.Seconds())
			v.HealthTimeout = int(p.HealthTimeout.Seconds())
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views, nil
}

// ContainerView is the API-facing representation of a container.
type ContainerView struct {
	Name          string            `json:"name"`
	ID            string            `json:"id"`
	Image         string            `json:"image"`
	ImageID       string            `json:"image_id"`
	Digests       []string          `json:"digests,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Networks      []string          `json:"networks,omitempty"`
	Status        string            `json:"status"`
	Health        string            `json:"health"`
	Running       bool              `json:"running"`
	RestartCount  int               `json:"restart_count"`
	Created       time.Time         `json:"created"`
	Pinned        bool              `json:"pinned"`
	AutoRemove    bool              `json:"auto_remove"`
	StopSignal    string            `json:"stop_signal,omitempty"`
	Managed       bool              `json:"managed"`
	MonitorOnly   bool              `json:"monitor_only"`
	Approve       bool              `json:"approve"`
	Rollback      bool              `json:"rollback"`
	Cleanup       bool              `json:"cleanup"`
	Schedule      string            `json:"schedule,omitempty"`
	Priority      int               `json:"priority"`
	DependsOn     []string          `json:"depends_on,omitempty"`
	HealthPort    int               `json:"health_port,omitempty"`
	HealthURL     string            `json:"health_url,omitempty"`
	StopTimeout   int               `json:"stop_timeout_seconds"`
	HealthTimeout int               `json:"health_timeout_seconds"`
}

// History returns recent history entries.
func (e *Engine) History(limit int) []state.Entry { return e.store.History(limit) }

// Pending returns updates awaiting approval.
func (e *Engine) Pending() []state.Pending { return e.store.Pending() }

// Metrics returns the metrics registry.
func (e *Engine) Metrics() *metrics.Registry { return e.metrics }

// Config returns the configuration the engine was built with.
func (e *Engine) Config() *config.Config { return e.cfg }

func networkNames(c *model.Container) []string {
	if c.Inspect.NetworkSettings == nil {
		return nil
	}
	out := make([]string, 0, len(c.Inspect.NetworkSettings.Networks))
	for name := range c.Inspect.NetworkSettings.Networks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) notifyReport(ctx context.Context, r *Report) {
	if !e.notify.Enabled() {
		return
	}
	nr := notify.Report{
		StartedAt: r.StartedAt,
		Duration:  r.Duration.Round(time.Millisecond).String(),
		DryRun:    r.DryRun,
	}
	for _, o := range r.Outcomes {
		switch o.Status {
		case state.StatusUpdated:
			nr.Updated++
		case state.StatusFailed:
			nr.Failed++
		case state.StatusRolledBack:
			nr.RolledBack++
		case state.StatusPending:
			nr.Pending++
		case state.StatusUpToDate:
			nr.UpToDate++
		}
		nr.Results = append(nr.Results, notify.Result{
			Container:  o.Container,
			Image:      o.Image,
			Status:     string(o.Status),
			OldImageID: o.OldImageID,
			NewImageID: o.NewImageID,
			Message:    o.Message,
			RolledBack: o.RolledBack,
		})
	}
	nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	e.notify.Send(nctx, nr)
}

func filterOnly(policies []policy.Policy, only []string) []policy.Policy {
	if len(only) == 0 {
		return policies
	}
	want := make(map[string]bool, len(only))
	for _, o := range only {
		want[strings.TrimSpace(o)] = true
	}
	out := policies[:0]
	for _, p := range policies {
		name := p.Container.Name()
		id := p.Container.ID()
		if want[name] || want[id] || hasPrefixIn(want, id) || hasPrefixIn(want, name) {
			out = append(out, p)
		}
	}
	return out
}

func hasPrefixIn(want map[string]bool, s string) bool {
	for w := range want {
		if w != "" && strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// digestsMatch compares local repo digests against the remote index digest and
// the platform-specific digest.
func digestsMatch(local []string, remote registry.Result) bool {
	if len(local) == 0 {
		return false
	}
	for _, d := range local {
		if remote.Digest != "" && d == remote.Digest {
			return true
		}
		if remote.PlatformDigest != "" && d == remote.PlatformDigest {
			return true
		}
	}
	return false
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
