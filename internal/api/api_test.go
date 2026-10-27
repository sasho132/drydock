package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/engine"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/state"
)

type fakeService struct {
	readyErr    error
	rollbackErr error

	lastRun    engine.Options
	runCalls   int
	rollbackTo string

	lastJobKind engine.JobKind
	jobErr      error
}

func (f *fakeService) Ready(context.Context) error { return f.readyErr }
func (f *fakeService) Containers(context.Context) ([]engine.ContainerView, error) {
	return []engine.ContainerView{{Name: "web", Image: "app:latest", Status: "running"}}, nil
}
func (f *fakeService) History(int) []state.Entry { return nil }
func (f *fakeService) Pending() []state.Pending  { return nil }
func (f *fakeService) Run(_ context.Context, opts engine.Options) (*engine.Report, error) {
	f.runCalls++
	f.lastRun = opts
	return &engine.Report{Outcomes: []engine.Outcome{{Container: "web", Status: state.StatusUpToDate}}}, nil
}
func (f *fakeService) Rollback(_ context.Context, name string) (engine.Outcome, error) {
	f.rollbackTo = name
	if f.rollbackErr != nil {
		return engine.Outcome{Container: name, Status: state.StatusFailed}, f.rollbackErr
	}
	return engine.Outcome{Container: name, Status: state.StatusRolledBack}, nil
}
func (f *fakeService) Metrics() *metrics.Registry { return metrics.New() }
func (f *fakeService) StartJob(kind engine.JobKind, opts engine.Options, container string) (engine.Job, error) {
	f.lastJobKind = kind
	if f.jobErr != nil {
		return engine.Job{}, f.jobErr
	}
	return engine.Job{ID: "job-1", Kind: kind, Status: engine.JobQueued, Container: container}, nil
}
func (f *fakeService) Job(id string) (engine.Job, bool) {
	if id != "job-1" {
		return engine.Job{}, false
	}
	return engine.Job{ID: id, Kind: engine.JobUpdate, Status: engine.JobSucceeded}, true
}
func (f *fakeService) Jobs(int) []engine.Job { return nil }

func newTestServer(svc Service, token string) *Server {
	cfg := config.Default()
	cfg.API.Token = token
	cfg.API.UI.Enabled = false
	return New(svc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHealthAndReadiness(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(newTestServer(svc, "").Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
	resp.Body.Close()

	svc.readyErr = context.DeadlineExceeded
	resp, _ = http.Get(srv.URL + "/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestContainersEndpoint(t *testing.T) {
	srv := httptest.NewServer(newTestServer(&fakeService{}, "").Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/containers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Containers []engine.ContainerView `json:"containers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Containers) != 1 || body.Containers[0].Name != "web" {
		t.Fatalf("body = %+v", body)
	}
}

func TestMutationsRequireToken(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(newTestServer(svc, "s3cret").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/update", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if svc.runCalls != 0 {
		t.Error("unauthorised request reached the engine")
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/update", strings.NewReader(`{"dry_run":true}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if svc.runCalls != 1 || !svc.lastRun.DryRun {
		t.Errorf("run was not invoked correctly: %+v", svc.lastRun)
	}
}

func TestUpdateAcceptsContainerList(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(newTestServer(svc, "").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/update", "application/json",
		strings.NewReader(`{"containers":["web","db"],"force":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(svc.lastRun.Only) != 2 || !svc.lastRun.Force {
		t.Errorf("options = %+v", svc.lastRun)
	}
}

func TestRollbackConflict(t *testing.T) {
	svc := &fakeService{rollbackErr: context.DeadlineExceeded}
	srv := httptest.NewServer(newTestServer(svc, "").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/rollback", "application/json", strings.NewReader(`{"container":"web"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	if svc.rollbackTo != "web" {
		t.Errorf("rollback target = %q", svc.rollbackTo)
	}
}

func TestRollbackRequiresContainer(t *testing.T) {
	srv := httptest.NewServer(newTestServer(&fakeService{}, "").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/rollback", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv := httptest.NewServer(newTestServer(&fakeService{}, "").Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "drydock_runs_total") {
		t.Errorf("metrics body = %s", b)
	}
}

func TestJobEndpoints(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(newTestServer(svc, "").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/jobs", "application/json", strings.NewReader(`{"kind":"check"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if svc.lastJobKind != engine.JobCheck {
		t.Errorf("kind = %q", svc.lastJobKind)
	}

	getResp, err := http.Get(srv.URL + "/v1/jobs/job-1")
	if err != nil {
		t.Fatal(err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", getResp.StatusCode)
	}

	missing, _ := http.Get(srv.URL + "/v1/jobs/nope")
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("missing job status = %d", missing.StatusCode)
	}
}

func TestJobBusyReturnsConflict(t *testing.T) {
	svc := &fakeService{jobErr: engine.ErrJobBusy}
	srv := httptest.NewServer(newTestServer(svc, "").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/jobs", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestJobMutationRequiresToken(t *testing.T) {
	srv := httptest.NewServer(newTestServer(&fakeService{}, "tok").Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/jobs", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
