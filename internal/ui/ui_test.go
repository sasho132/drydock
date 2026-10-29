package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/engine"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/model"
	"github.com/sasho/drydock/internal/state"
)

type fakeService struct {
	containers []engine.ContainerView
	history    []state.Entry
	pending    []state.Pending
	job        engine.Job
	jobOK      bool
	startErr   error

	startCalls     int
	startKind      engine.JobKind
	startContainer string
	startOpts      engine.Options
}

func (f *fakeService) Ready(context.Context) error { return nil }
func (f *fakeService) Containers(context.Context) ([]engine.ContainerView, error) {
	return f.containers, nil
}
func (f *fakeService) History(int) []state.Entry { return f.history }
func (f *fakeService) Pending() []state.Pending  { return f.pending }
func (f *fakeService) Run(context.Context, engine.Options) (*engine.Report, error) {
	return &engine.Report{}, nil
}
func (f *fakeService) Rollback(context.Context, string) (engine.Outcome, error) {
	return engine.Outcome{}, nil
}
func (f *fakeService) StartJob(kind engine.JobKind, opts engine.Options, container string) (engine.Job, error) {
	f.startCalls++
	f.startKind = kind
	f.startContainer = container
	f.startOpts = opts
	if f.startErr != nil {
		return engine.Job{}, f.startErr
	}
	return engine.Job{ID: "job-1", Kind: kind, Status: engine.JobQueued, Container: container}, nil
}
func (f *fakeService) Job(id string) (engine.Job, bool) {
	if !f.jobOK || id != "job-1" {
		return engine.Job{}, false
	}
	return f.job, true
}
func (f *fakeService) Jobs(int) []engine.Job { return nil }
func (f *fakeService) Metrics() *metrics.Registry {
	return metrics.New()
}

func fixture() *fakeService {
	now := time.Now()
	return &fakeService{
		containers: []engine.ContainerView{{
			Name: "web", ID: "web-id", Image: "app:latest", ImageID: "sha256:old",
			Digests: []string{"sha256:aaa"}, Networks: []string{"bridge"},
			Labels: map[string]string{"drydock.monitor-only": "false"},
			Status: "running", Health: "healthy", Running: true,
			Managed: true, Rollback: true, StopTimeout: 30, HealthTimeout: 120,
		}},
		history: []state.Entry{{
			Time: now.Add(-time.Hour), Container: "web", Image: "app:latest",
			Status: state.StatusUpdated, OldImageID: "sha256:old", NewImageID: "sha256:new",
			Snapshot: &model.Snapshot{ImageID: "sha256:old"},
		}},
		pending: []state.Pending{{Container: "web", Image: "app:latest", DetectedAt: now}},
		job:     engine.Job{ID: "job-1", Kind: engine.JobUpdate, Status: engine.JobSucceeded},
		jobOK:   true,
	}
}

func testServer(t *testing.T, password string) (*Server, *fakeService) {
	t.Helper()
	cfg := config.Default()
	cfg.API.UI = config.UIConfig{Enabled: true, Username: "admin", Password: password}
	svc := fixture()
	return New(svc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), svc
}

func sessionFor(s *Server, csrf string) *http.Cookie {
	return &http.Cookie{
		Name:  sessionCookie,
		Value: s.encodeSession(&session{User: "admin", CSRF: csrf, Exp: time.Now().Add(time.Hour).Unix()}),
	}
}

func TestDashboardRenders(t *testing.T) {
	s, _ := testServer(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessionFor(s, "csrf-token"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Dashboard", "web", "Pending approvals", "Recent activity", "Rollback", "table responsive", `data-label="Container"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("CSP = %q", got)
	}
}

func TestReadOnlyModeDisablesActions(t *testing.T) {
	s, svc := testServer(t, "")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "Read-only dashboard") {
		t.Error("expected the read-only banner")
	}

	form := url.Values{"container": {"web"}, "next": {"/"}}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/actions/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("location = %q, want an error flash", loc)
	}
	if svc.startCalls != 0 {
		t.Error("read-only mode must not start jobs")
	}
}

func TestLoginFlow(t *testing.T) {
	s, _ := testServer(t, "secret")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("expected redirect to login, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password status = %d", resp.StatusCode)
	}

	resp, err = client.PostForm(ts.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"secret"}, "next": {"/containers"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/containers" {
		t.Fatalf("login status = %d location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "drydock_ui" {
			sc = c
		}
	}
	if sc == nil || sc.Value == "" {
		t.Fatal("session cookie not set")
	}
	if !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags = %+v", sc)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.AddCookie(sc)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated dashboard = %d", resp.StatusCode)
	}
}

func TestActionRequiresCSRF(t *testing.T) {
	s, svc := testServer(t, "secret")
	form := url.Values{"container": {"web"}, "next": {"/containers"}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/actions/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionFor(s, "right-token"))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", rec.Code)
	}
	if svc.startCalls != 0 {
		t.Error("request without CSRF must not start a job")
	}

	form.Set("csrf", "right-token")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/actions/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionFor(s, "right-token"))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "job=job-1") {
		t.Errorf("location = %q", loc)
	}
	if svc.startCalls != 1 || svc.startKind != engine.JobUpdate {
		t.Fatalf("start calls = %d kind = %s", svc.startCalls, svc.startKind)
	}
	if len(svc.startOpts.Only) != 1 || svc.startOpts.Only[0] != "web" {
		t.Errorf("options = %+v", svc.startOpts)
	}
}

func TestActionBusyShowsError(t *testing.T) {
	s, svc := testServer(t, "secret")
	svc.startErr = engine.ErrJobBusy

	form := url.Values{"csrf": {"t"}, "container": {"web"}, "next": {"/"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/actions/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionFor(s, "t"))
	s.Handler().ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("location = %q", loc)
	}
}

func TestRollbackAction(t *testing.T) {
	s, svc := testServer(t, "secret")
	form := url.Values{"csrf": {"t"}, "container": {"web"}, "next": {"/"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/actions/rollback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionFor(s, "t"))
	s.Handler().ServeHTTP(rec, req)
	if svc.startKind != engine.JobRollback || svc.startContainer != "web" {
		t.Fatalf("kind = %s container = %q", svc.startKind, svc.startContainer)
	}
}

func TestContainerDetail(t *testing.T) {
	s, _ := testServer(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/containers/view?name=web", nil)
	req.AddCookie(sessionFor(s, "t"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{"Policy", "sha256:aaa", "Automatic rollback", "drydock.monitor-only", "bridge"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("detail missing %q", want)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/containers/view?name=missing", nil)
	req.AddCookie(sessionFor(s, "t"))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing container status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Not found") {
		t.Errorf("missing container error page: %s", rec.Body.String())
	}
}

func TestHistoryFiltering(t *testing.T) {
	s, _ := testServer(t, "secret")
	req := httptest.NewRequest(http.MethodGet, "/history?status=updated", nil)
	req.AddCookie(sessionFor(s, "t"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "web") {
		t.Fatalf("status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/history?status=failed", nil)
	req.AddCookie(sessionFor(s, "t"))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "app:latest") {
		t.Error("failed filter should not show the updated entry")
	}
}

func TestStaticAssets(t *testing.T) {
	s, _ := testServer(t, "secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "max-age") {
		t.Errorf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), "--accent") {
		t.Error("stylesheet content missing")
	}
}

func TestJobEndpointAuth(t *testing.T) {
	s, _ := testServer(t, "secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jobs/job-1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous job poll = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs/job-1", nil)
	req.AddCookie(sessionFor(s, "t"))
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "job-1") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"/containers":       "/containers",
		"/history?x=1":      "/history?x=1",
		"":                  "/",
		"//evil.example":    "/",
		"https://evil.test": "/",
		"/\\evil":           "/",
		"containers":        "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenRedirectRejected(t *testing.T) {
	s, _ := testServer(t, "secret")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.PostForm(ts.URL+"/login", url.Values{
		"username": {"admin"}, "password": {"secret"}, "next": {"//evil.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("open redirect not blocked: %q", loc)
	}
}
