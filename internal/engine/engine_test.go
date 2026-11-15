package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/docker"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/model"
	"github.com/sasho/drydock/internal/notify"
	"github.com/sasho/drydock/internal/registry"
	"github.com/sasho/drydock/internal/state"
)

type fakeDocker struct {
	containers []*model.Container

	pullID    string
	pullErr   error
	pullCalls int

	recreateFn    func(c *model.Container, opts docker.RecreateOptions) (docker.RecreateResult, error)
	recreateCalls int
	lastRecreate  docker.RecreateOptions

	removed      []string
	rollbackFn   func(current *model.Container, target model.Snapshot) (string, error)
	rollbackCall int
}

func (f *fakeDocker) Ping(context.Context) error                       { return nil }
func (f *fakeDocker) List(context.Context) ([]*model.Container, error) { return f.containers, nil }
func (f *fakeDocker) HasImage(context.Context, string) bool            { return true }

func (f *fakeDocker) Inspect(_ context.Context, id string) (*model.Container, error) {
	for _, c := range f.containers {
		if c.ID() == id {
			return c, nil
		}
	}
	return nil, docker.ErrNotFound
}

func (f *fakeDocker) Pull(context.Context, string, string) (string, error) {
	f.pullCalls++
	return f.pullID, f.pullErr
}

func (f *fakeDocker) Recreate(_ context.Context, c *model.Container, opts docker.RecreateOptions) (docker.RecreateResult, error) {
	f.recreateCalls++
	f.lastRecreate = opts
	if f.recreateFn != nil {
		return f.recreateFn(c, opts)
	}
	return docker.RecreateResult{OldContainerID: c.ID(), NewContainerID: "new-container", Snapshot: c.Snapshot()}, nil
}

func (f *fakeDocker) Rollback(_ context.Context, current *model.Container, target model.Snapshot, _ docker.HealthOptions) (string, error) {
	f.rollbackCall++
	if f.rollbackFn != nil {
		return f.rollbackFn(current, target)
	}
	return "rolled-back-container", nil
}

func (f *fakeDocker) RemoveImage(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

type fakeResolver struct {
	result registry.Result
	err    error
	auth   string
	block  <-chan struct{}
}

func (f *fakeResolver) Resolve(context.Context, string, registry.Platform) (registry.Result, error) {
	if f.block != nil {
		<-f.block
	}
	return f.result, f.err
}
func (f *fakeResolver) DockerAuth(string) string { return f.auth }

func testContainer(name, ref, imageID, digest string, labels map[string]string) *model.Container {
	return &model.Container{
		Inspect: container.InspectResponse{
			ID:         name + "-id",
			Name:       "/" + name,
			Image:      imageID,
			Config:     &container.Config{Image: ref, Labels: labels},
			HostConfig: &container.HostConfig{},
			State:      &container.State{Running: true, Status: container.StateRunning},
		},
		ImageInspect: image.InspectResponse{
			ID:          imageID,
			RepoDigests: []string{ref + "@" + digest},
		},
	}
}

type harness struct {
	engine   *Engine
	store    *state.Store
	docker   *fakeDocker
	resolver *fakeResolver
	cfg      *config.Config
}

func newHarness(t *testing.T, containers ...*model.Container) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Concurrency = 1
	store, err := state.Open(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDocker{containers: containers}
	r := &fakeResolver{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(cfg, d, r, store, notify.New(nil, log), metrics.New(), log)
	return &harness{engine: eng, store: store, docker: d, resolver: r, cfg: cfg}
}

func TestRunUpToDateDoesNotPull(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.result = registry.Result{Digest: "sha256:aaa", PlatformDigest: "sha256:aaa"}

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].Status != state.StatusUpToDate {
		t.Fatalf("outcomes = %+v", report.Outcomes)
	}
	if h.docker.pullCalls != 0 {
		t.Errorf("pull should not happen for an up-to-date image")
	}
	if got := len(h.store.History(0)); got != 0 {
		t.Errorf("history should stay empty for scans, got %d", got)
	}
}

func TestRunUpdatesAndRecordsRollbackPoint(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.result = registry.Result{Digest: "sha256:bbb", PlatformDigest: "sha256:bbb"}
	h.resolver.auth = "auth-blob"
	h.docker.pullID = "sha256:new"

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v", report.Outcomes)
	}
	out := report.Outcomes[0]
	if out.Status != state.StatusUpdated {
		t.Fatalf("status = %s (%s)", out.Status, out.Message)
	}
	if h.docker.pullCalls != 1 || h.docker.recreateCalls != 1 {
		t.Errorf("pull=%d recreate=%d", h.docker.pullCalls, h.docker.recreateCalls)
	}
	if !h.docker.lastRecreate.Rollback {
		t.Error("rollback should be enabled by default")
	}
	if h.docker.lastRecreate.ImageRef != "app:latest" {
		t.Errorf("image ref = %q", h.docker.lastRecreate.ImageRef)
	}

	entry, ok := h.store.LastSuccessful("web")
	if !ok {
		t.Fatal("expected a rollback point")
	}
	if entry.Snapshot == nil || entry.Snapshot.ImageID != "sha256:old" {
		t.Fatalf("snapshot = %+v", entry.Snapshot)
	}
}

func TestRunDryRunDoesNotPull(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.result = registry.Result{Digest: "sha256:bbb"}

	report, err := h.engine.Run(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcomes[0].Status != state.StatusPending {
		t.Fatalf("status = %s", report.Outcomes[0].Status)
	}
	if h.docker.pullCalls != 0 {
		t.Error("dry run must not pull")
	}
	if len(h.store.Pending()) != 1 {
		t.Error("dry run should record a pending update")
	}
}

func TestRunMonitorOnlyDoesNotUpdate(t *testing.T) {
	h := newHarness(t, testContainer("db", "postgres:16", "sha256:old", "sha256:aaa",
		map[string]string{"drydock.monitor-only": "true"}))
	h.resolver.result = registry.Result{Digest: "sha256:bbb"}

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcomes[0].Status != state.StatusPending {
		t.Fatalf("status = %s", report.Outcomes[0].Status)
	}
	if h.docker.pullCalls != 0 {
		t.Error("monitor-only must not pull")
	}
}

func TestRunApprovalRequiresForce(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa",
		map[string]string{"drydock.approve": "true"}))
	h.resolver.result = registry.Result{Digest: "sha256:bbb"}
	h.docker.pullID = "sha256:new"

	report, _ := h.engine.Run(context.Background(), Options{})
	if report.Outcomes[0].Status != state.StatusPending {
		t.Fatalf("expected pending approval, got %s", report.Outcomes[0].Status)
	}

	report, _ = h.engine.Run(context.Background(), Options{Force: true})
	if report.Outcomes[0].Status != state.StatusUpdated {
		t.Fatalf("expected update when forced, got %s", report.Outcomes[0].Status)
	}
	if got := len(h.store.Pending()); got != 0 {
		t.Fatalf("pending after forced update = %d", got)
	}
}

func TestRunUpToDateClearsStalePending(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.result = registry.Result{Digest: "sha256:aaa"}
	if err := h.store.SetPending(state.Pending{Container: "web", Image: "app:latest"}); err != nil {
		t.Fatal(err)
	}

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcomes[0].Status != state.StatusUpToDate {
		t.Fatalf("status = %s", report.Outcomes[0].Status)
	}
	if got := len(h.store.Pending()); got != 0 {
		t.Fatalf("stale pending entry survived: %d", got)
	}
}

func TestRunRollsBackOnFailure(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.result = registry.Result{Digest: "sha256:bbb"}
	h.docker.pullID = "sha256:new"
	h.docker.recreateFn = func(c *model.Container, _ docker.RecreateOptions) (docker.RecreateResult, error) {
		return docker.RecreateResult{
			OldContainerID: c.ID(),
			Snapshot:       c.Snapshot(),
			RolledBack:     true,
		}, context.DeadlineExceeded
	}

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := report.Outcomes[0]
	if out.Status != state.StatusRolledBack || !out.RolledBack {
		t.Fatalf("outcome = %+v", out)
	}
	if entries := h.store.History(0); len(entries) != 1 || entries[0].Status != state.StatusRolledBack {
		t.Fatalf("history = %+v", entries)
	}
}

func TestRunSkipsPinnedImage(t *testing.T) {
	c := testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil)
	c.Inspect.Config.Image = "app@sha256:" + repeat("a", 64)
	h := newHarness(t, c)

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcomes[0].Status != state.StatusUpToDate {
		t.Fatalf("status = %s", report.Outcomes[0].Status)
	}
}

func TestRunRegistryErrorIsReported(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.err = context.DeadlineExceeded

	report, err := h.engine.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcomes[0].Status != state.StatusFailed {
		t.Fatalf("status = %s", report.Outcomes[0].Status)
	}
}

func TestRollbackUsesLastSnapshot(t *testing.T) {
	c := testContainer("web", "app:latest", "sha256:new", "sha256:bbb", nil)
	h := newHarness(t, c)
	snap := &model.Snapshot{ContainerID: c.ID(), Name: "web", ImageRef: "app:latest", ImageID: "sha256:old"}
	if err := h.store.Append(state.Entry{Container: "web", Status: state.StatusUpdated, Snapshot: snap}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetPending(state.Pending{Container: "web", Image: "app:latest"}); err != nil {
		t.Fatal(err)
	}

	out, err := h.engine.Rollback(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != state.StatusRolledBack {
		t.Fatalf("status = %s", out.Status)
	}
	if h.docker.rollbackCall != 1 {
		t.Errorf("rollback calls = %d", h.docker.rollbackCall)
	}
	if got := len(h.store.Pending()); got != 0 {
		t.Fatalf("pending after rollback = %d", got)
	}
}

func TestRollbackWithoutHistoryFails(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:new", "sha256:bbb", nil))
	if _, err := h.engine.Rollback(context.Background(), "web"); err == nil {
		t.Fatal("expected an error when no rollback point exists")
	}
}

func TestFilterOnly(t *testing.T) {
	c1 := testContainer("web", "app:latest", "id1", "sha256:a", nil)
	c2 := testContainer("db", "postgres:16", "id2", "sha256:b", nil)
	h := newHarness(t, c1, c2)
	h.resolver.result = registry.Result{Digest: "sha256:c"}

	report, err := h.engine.Run(context.Background(), Options{Only: []string{"db"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].Container != "db" {
		t.Fatalf("outcomes = %+v", report.Outcomes)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
