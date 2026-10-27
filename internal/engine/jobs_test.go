package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sasho/drydock/internal/model"
	"github.com/sasho/drydock/internal/registry"
	"github.com/sasho/drydock/internal/state"
)

func TestCheckJobSucceeds(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.result = registry.Result{Digest: "sha256:aaa"}

	job, err := h.engine.StartJob(JobCheck, Options{}, "")
	if err != nil {
		t.Fatal(err)
	}
	done, ok := h.engine.waitForJob(job.ID, 2*time.Second)
	if !ok {
		t.Fatal("job disappeared")
	}
	if done.Status != JobSucceeded {
		t.Fatalf("status = %s, error = %s", done.Status, done.Error)
	}
	if done.Report == nil || len(done.Report.Outcomes) != 1 {
		t.Fatalf("report = %+v", done.Report)
	}
	if done.Report.Outcomes[0].Status != state.StatusUpToDate {
		t.Errorf("outcome = %+v", done.Report.Outcomes[0])
	}
	if done.FinishedAt == nil {
		t.Error("FinishedAt not set")
	}
}

func TestJobRejectsSecondRun(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	block := make(chan struct{})
	h.resolver.block = block
	h.resolver.result = registry.Result{Digest: "sha256:aaa"}

	job, err := h.engine.StartJob(JobCheck, Options{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StartJob(JobUpdate, Options{}, ""); !errors.Is(err, ErrJobBusy) {
		t.Fatalf("expected ErrJobBusy, got %v", err)
	}
	close(block)
	if done, _ := h.engine.waitForJob(job.ID, 2*time.Second); done.Status != JobSucceeded {
		t.Fatalf("first job status = %s", done.Status)
	}
	if h.engine.JobBusy() {
		t.Error("engine still reports a busy job")
	}
}

func TestJobValidation(t *testing.T) {
	h := newHarness(t)
	if _, err := h.engine.StartJob("bogus", Options{}, ""); err == nil {
		t.Error("expected an error for an unknown kind")
	}
	if _, err := h.engine.StartJob(JobRollback, Options{}, ""); err == nil {
		t.Error("expected an error for rollback without a container")
	}
}

func TestRollbackJob(t *testing.T) {
	c := testContainer("web", "app:latest", "sha256:new", "sha256:bbb", nil)
	h := newHarness(t, c)
	snap := &model.Snapshot{ContainerID: c.ID(), Name: "web", ImageRef: "app:latest", ImageID: "sha256:old"}
	if err := h.store.Append(state.Entry{Container: "web", Status: state.StatusUpdated, Snapshot: snap}); err != nil {
		t.Fatal(err)
	}

	job, err := h.engine.StartJob(JobRollback, Options{}, "web")
	if err != nil {
		t.Fatal(err)
	}
	done, _ := h.engine.waitForJob(job.ID, 2*time.Second)
	if done.Status != JobSucceeded || done.Outcome == nil {
		t.Fatalf("job = %+v", done)
	}
	if done.Outcome.Status != state.StatusRolledBack {
		t.Errorf("outcome = %+v", done.Outcome)
	}
	if h.docker.rollbackCall != 1 {
		t.Errorf("rollback calls = %d", h.docker.rollbackCall)
	}
}

func TestJobFailureIsReported(t *testing.T) {
	h := newHarness(t, testContainer("web", "app:latest", "sha256:old", "sha256:aaa", nil))
	h.resolver.err = context.DeadlineExceeded
	h.docker.pullID = "sha256:new"

	job, err := h.engine.StartJob(JobUpdate, Options{}, "")
	if err != nil {
		t.Fatal(err)
	}
	done, _ := h.engine.waitForJob(job.ID, 2*time.Second)
	// A registry failure is recorded per container, not as a job failure; the
	// job itself completes. The report carries the failure.
	if done.Status != JobSucceeded {
		t.Fatalf("status = %s", done.Status)
	}
	if done.Report == nil || done.Report.Outcomes[0].Status != state.StatusFailed {
		t.Fatalf("report = %+v", done.Report)
	}
}
