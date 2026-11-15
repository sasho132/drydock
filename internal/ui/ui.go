// Package ui serves drydock's embedded web dashboard.
//
// It is deliberately server-rendered: templates and static assets are embedded
// in the binary, so the container image stays a single static executable with
// no Node.js build step and no external CDN at runtime.
package ui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/engine"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/state"
	"github.com/sasho/drydock/internal/version"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Service is the engine surface the dashboard needs.
type Service interface {
	Ready(ctx context.Context) error
	Containers(ctx context.Context) ([]engine.ContainerView, error)
	History(limit int) []state.Entry
	Pending() []state.Pending
	Run(ctx context.Context, opts engine.Options) (*engine.Report, error)
	Rollback(ctx context.Context, name string) (engine.Outcome, error)
	StartJob(kind engine.JobKind, opts engine.Options, container string) (engine.Job, error)
	Job(id string) (engine.Job, bool)
	Jobs(limit int) []engine.Job
	Metrics() *metrics.Registry
}

const (
	sessionCookie = "drydock_ui"
	sessionTTL    = 12 * time.Hour
)

var pages = []string{"dashboard", "containers", "container", "history", "settings", "login", "error"}

// The production engine must satisfy the dashboard interface.
var _ Service = (*engine.Engine)(nil)

// Server renders the dashboard.
type Server struct {
	svc     Service
	cfg     *config.Config
	log     *slog.Logger
	tmpl    map[string]*template.Template
	secret  []byte
	limiter *limiter
}

// New builds the dashboard server. Templates are embedded, so a parse failure
// is a build-time bug and panics loudly during tests.
func New(svc Service, cfg *config.Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		svc:     svc,
		cfg:     cfg,
		log:     log,
		tmpl:    map[string]*template.Template{},
		limiter: newLimiter(8, 5*time.Minute),
	}
	if cfg.API.UI.AuthEnabled() {
		// Deriving the signing key from the password means changing the
		// password immediately invalidates every existing session.
		sum := sha256.Sum256([]byte("drydock-ui-session:" + cfg.API.UI.Password))
		s.secret = sum[:]
	}

	funcs := template.FuncMap{
		"ago":         humanAgo,
		"datetime":    func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
		"rfc3339":     func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"short":       shortID,
		"statusClass": statusClass,
		"healthClass": healthClass,
		"join":        strings.Join,
		"add":         func(a, b int) int { return a + b },
		"dash": func(s string) string {
			if s == "" {
				return "—"
			}
			return s
		},
	}
	for _, p := range pages {
		t := template.New(p).Funcs(funcs)
		t = template.Must(t.ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+p+".html"))
		s.tmpl[p] = t
	}
	return s
}

// Handler returns the dashboard HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.withSession(s.dashboard))
	mux.HandleFunc("GET /containers", s.withSession(s.containers))
	mux.HandleFunc("GET /containers/view", s.withSession(s.containerDetail))
	mux.HandleFunc("GET /history", s.withSession(s.history))
	mux.HandleFunc("GET /settings", s.withSession(s.settings))
	mux.HandleFunc("GET /jobs/{id}", s.withSession(s.jobJSON))

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.withSession(s.logout))

	mux.HandleFunc("POST /actions/check", s.action(engine.JobCheck))
	mux.HandleFunc("POST /actions/update", s.action(engine.JobUpdate))
	mux.HandleFunc("POST /actions/rollback", s.action(engine.JobRollback))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("ui: embedded static assets missing: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(sub)))))

	return s.securityHeaders(mux)
}

// --- pages -----------------------------------------------------------------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sess *session) {
	v := s.base(r, sess, "Dashboard", "dashboard")
	containers, err := s.svc.Containers(r.Context())
	if err != nil {
		s.fail(w, r, v, "Could not list containers: "+err.Error())
		return
	}
	all := s.svc.History(0)
	rows := s.rows(containers, all)
	pending := s.svc.Pending()

	v.Summary = s.summary(containers, all, pending)
	v.Pending = pending
	v.History = historyRows(all, 8)
	if len(rows) > 10 {
		v.Containers = rows[:10]
		v.MoreContainers = len(rows) - 10
	} else {
		v.Containers = rows
	}
	s.render(w, r, "dashboard", v)
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request, sess *session) {
	v := s.base(r, sess, "Containers", "containers")
	v.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	containers, err := s.svc.Containers(r.Context())
	if err != nil {
		s.fail(w, r, v, "Could not list containers: "+err.Error())
		return
	}
	rows := s.rows(containers, s.svc.History(0))
	if v.Query != "" {
		rows = filterRows(rows, v.Query)
	}
	v.Containers = rows
	s.render(w, r, "containers", v)
}

func (s *Server) containerDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	v := s.base(r, sess, "Container", "containers")
	if name == "" {
		http.Redirect(w, r, "/containers", http.StatusSeeOther)
		return
	}
	containers, err := s.svc.Containers(r.Context())
	if err != nil {
		s.fail(w, r, v, "Could not list containers: "+err.Error())
		return
	}
	all := s.svc.History(0)
	rows := s.rows(containers, all)
	var found *containerRow
	for i := range rows {
		if rows[i].Name == name {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		v.Status = http.StatusNotFound
		s.fail(w, r, v, fmt.Sprintf("Container %q was not found.", name))
		return
	}
	var view engine.ContainerView
	for _, c := range containers {
		if c.Name == name {
			view = c
			break
		}
	}

	detail := &containerDetail{
		containerRow: *found,
		ID:           view.ID,
		Digests:      view.Digests,
		Labels:       view.Labels,
		Networks:     view.Networks,
		Created:      view.Created,
		RestartCount: view.RestartCount,
		AutoRemove:   view.AutoRemove,
		StopSignal:   view.StopSignal,
		History:      historyRowsFor(all, name, ""),
		Policy: policyView{
			Rollback:      view.Rollback,
			Cleanup:       view.Cleanup,
			Priority:      view.Priority,
			DependsOn:     view.DependsOn,
			HealthPort:    view.HealthPort,
			HealthURL:     view.HealthURL,
			StopTimeout:   view.StopTimeout,
			HealthTimeout: view.HealthTimeout,
			Schedule:      view.Schedule,
			Approve:       view.Approve,
			MonitorOnly:   view.MonitorOnly,
		},
	}
	detail.LabelKeys = make([]string, 0, len(view.Labels))
	for k := range view.Labels {
		detail.LabelKeys = append(detail.LabelKeys, k)
	}
	sort.Strings(detail.LabelKeys)
	v.Detail = detail
	v.Title = name
	s.render(w, r, "container", v)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, sess *session) {
	v := s.base(r, sess, "History", "history")
	v.Query = strings.TrimSpace(r.URL.Query().Get("container"))
	v.FilterStatus = strings.TrimSpace(r.URL.Query().Get("status"))
	v.Limit = 200
	all := s.svc.History(0)
	rows := historyRowsFor(all, v.Query, v.FilterStatus)
	if len(rows) > v.Limit {
		rows = rows[:v.Limit]
	}
	v.History = rows
	s.render(w, r, "history", v)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request, sess *session) {
	v := s.base(r, sess, "Settings", "settings")
	v.Settings = buildSettings(s.cfg)
	s.render(w, r, "settings", v)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, v view, msg string) {
	v.Error = msg
	if v.Status == 0 {
		v.Status = http.StatusInternalServerError
	}
	if v.Status == http.StatusNotFound {
		v.Title = "Not found"
	} else {
		v.Title = "Error"
	}
	s.render(w, r, "error", v)
}

// --- auth ------------------------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.API.UI.AuthEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	v := s.base(r, nil, "Sign in", "login")
	v.Login = true
	v.Next = safeNext(r.URL.Query().Get("next"))
	s.render(w, r, "login", v)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.API.UI.AuthEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		v := s.base(r, nil, "Sign in", "login")
		v.Login = true
		v.Status = http.StatusTooManyRequests
		v.Error = "Too many attempts. Try again in a few minutes."
		s.render(w, r, "login", v)
		return
	}

	user := strings.TrimSpace(r.PostFormValue("username"))
	pass := r.PostFormValue("password")
	wantUser := s.cfg.API.UI.Username
	if wantUser == "" {
		wantUser = "admin"
	}
	if !secureEqual(user, wantUser) || !secureEqual(pass, s.cfg.API.UI.Password) {
		// A small delay makes online guessing less pleasant without holding a
		// connection open long enough to be a DoS vector.
		time.Sleep(250 * time.Millisecond)
		v := s.base(r, nil, "Sign in", "login")
		v.Login = true
		v.Status = http.StatusUnauthorized
		v.Error = "Invalid username or password."
		v.Next = safeNext(r.PostFormValue("next"))
		s.render(w, r, "login", v)
		return
	}

	s.limiter.reset(ip)
	sess := &session{User: user, CSRF: randomToken(), Exp: time.Now().Add(sessionTTL).Unix()}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.encodeSession(sess),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.csrfOK(r, sess) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type session struct {
	User string `json:"u"`
	CSRF string `json:"c"`
	Exp  int64  `json:"e"`
}

func (s *Server) encodeSession(sess *session) string {
	payload, _ := json.Marshal(sess)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) decodeSession(raw string) (*session, bool) {
	payloadB64, sigB64, ok := strings.Cut(raw, ".")
	if !ok {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return nil, false
	}
	var sess session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return nil, false
	}
	if time.Now().Unix() > sess.Exp {
		return nil, false
	}
	return &sess, true
}

func (s *Server) session(r *http.Request) (*session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	return s.decodeSession(c.Value)
}

func (s *Server) withSession(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if s.cfg.API.UI.AuthEnabled() && !ok {
			if strings.HasPrefix(r.URL.Path, "/jobs/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, r, sess)
	}
}

func (s *Server) csrfOK(r *http.Request, sess *session) bool {
	if !s.cfg.API.UI.AuthEnabled() || sess == nil {
		return false
	}
	got := r.PostFormValue("csrf")
	if got == "" {
		got = r.Header.Get("X-CSRF-Token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) == 1
}

// --- actions ---------------------------------------------------------------

func (s *Server) action(kind engine.JobKind) http.HandlerFunc {
	return s.withSession(func(w http.ResponseWriter, r *http.Request, sess *session) {
		back := safeNext(r.PostFormValue("next"))
		if !s.cfg.API.UI.AuthEnabled() {
			s.redirectFlash(w, r, back, "", "Dashboard actions are disabled. Set api.ui.password to enable them.")
			return
		}
		if err := r.ParseForm(); err != nil || !s.csrfOK(r, sess) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		container := strings.TrimSpace(r.PostFormValue("container"))
		opts := engine.Options{}
		if container != "" {
			opts.Only = []string{container}
		}
		// A manual click is the approval. Pending updates carry this flag so a
		// container with drydock.approve=true can actually be applied; without
		// it the engine would just record the same pending update again.
		if kind == engine.JobUpdate {
			if approve, err := strconv.ParseBool(strings.TrimSpace(r.PostFormValue("approve"))); err == nil {
				opts.Force = approve
			}
		}
		job, err := s.svc.StartJob(kind, opts, container)
		if err != nil {
			s.redirectFlash(w, r, back, "", err.Error())
			return
		}
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		http.Redirect(w, r, back+sep+"job="+url.QueryEscape(job.ID), http.StatusSeeOther)
	})
}

func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, back, notice, errMsg string) {
	q := url.Values{}
	if notice != "" {
		q.Set("notice", notice)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		back += sep + q.Encode()
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) jobJSON(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	job, ok := s.svc.Job(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// --- view models -----------------------------------------------------------

type view struct {
	Title   string
	Nav     string
	Version string
	Path    string
	Query   string

	LabelPrefix    string
	GlobalInterval string

	User        string
	CSRF        string
	Notice      string
	Error       string
	ReadOnly    bool
	AuthEnabled bool
	CanAct      bool
	Login       bool
	Next        string

	JobID      string
	JobRunning bool

	Status int

	Summary        summary
	Containers     []containerRow
	MoreContainers int
	Pending        []state.Pending
	History        []historyRow
	Detail         *containerDetail
	Settings       *settingsView
	Limit          int
	FilterStatus   string
}

type summary struct {
	Total, Managed, Running, Pending     int
	Updated24h, Failed24h, RolledBack24h int
	LastRun                              time.Time
	LastRunFor                           string
	Runs                                 uint64
}

type containerRow struct {
	Name        string
	Image       string
	ImageID     string
	Status      string
	Health      string
	Schedule    string
	LastStatus  string
	LastTime    time.Time
	Running     bool
	Managed     bool
	MonitorOnly bool
	Approve     bool
	Pinned      bool
	CanRollback bool
}

type containerDetail struct {
	containerRow
	ID           string
	Digests      []string
	Labels       map[string]string
	LabelKeys    []string
	Networks     []string
	Created      time.Time
	RestartCount int
	AutoRemove   bool
	StopSignal   string
	Policy       policyView
	History      []historyRow
}

type policyView struct {
	Rollback      bool
	Cleanup       bool
	Priority      int
	DependsOn     []string
	HealthPort    int
	HealthURL     string
	StopTimeout   int
	HealthTimeout int
	Schedule      string
	Approve       bool
	MonitorOnly   bool
}

type historyRow struct {
	Time      time.Time
	Container string
	Image     string
	Status    string
	Message   string
	OldImage  string
	NewImage  string
}

type settingsView struct {
	Interval       string
	Schedule       string
	Scope          string
	StateFile      string
	LogLevel       string
	LogFormat      string
	Concurrency    int
	KeepImages     int
	Cleanup        bool
	Rollback       bool
	DryRun         bool
	LabelEnable    bool
	LabelPrefix    string
	MonitorOnly    bool
	IncludeStopped bool
	RemoveVolumes  bool
	StopTimeout    string
	HealthTimeout  string
	Settle         string

	APITokenSet bool
	APIAddr     string
	UIEnabled   bool
	UIAuth      bool
	UIUser      string

	Registries    []registryRow
	Notifications []notifRow
	Overrides     []overrideRow
}

type registryRow struct {
	Host     string
	Username string
}

type notifRow struct {
	Type   string
	Target string
	Events string
}

type overrideRow struct {
	Name    string
	Summary string
}

func (s *Server) base(r *http.Request, sess *session, title, nav string) view {
	v := view{
		Title:   title,
		Nav:     nav,
		Version: version.Version,
		Path:    r.URL.RequestURI(),

		LabelPrefix:    s.cfg.LabelPrefix,
		GlobalInterval: s.cfg.Interval.Duration.String(),
	}
	v.AuthEnabled = s.cfg.API.UI.AuthEnabled()
	v.ReadOnly = !v.AuthEnabled
	v.CanAct = v.AuthEnabled
	if sess != nil {
		v.User = sess.User
		v.CSRF = sess.CSRF
	}
	v.Notice = strings.TrimSpace(r.URL.Query().Get("notice"))
	v.Error = strings.TrimSpace(r.URL.Query().Get("error"))

	if id := r.URL.Query().Get("job"); id != "" {
		if job, ok := s.svc.Job(id); ok {
			v.JobID = job.ID
			v.JobRunning = job.Running()
		}
	}
	for _, j := range s.svc.Jobs(1) {
		if j.Running() {
			v.JobRunning = true
		}
	}
	return v
}

func (s *Server) rows(containers []engine.ContainerView, history []state.Entry) []containerRow {
	last := map[string]state.Entry{}
	rollback := map[string]bool{}
	for _, e := range history { // newest first
		if _, ok := last[e.Container]; !ok {
			last[e.Container] = e
		}
		if e.Status == state.StatusUpdated && e.Snapshot != nil {
			rollback[e.Container] = true
		}
	}
	out := make([]containerRow, 0, len(containers))
	for _, c := range containers {
		row := containerRow{
			Name:        c.Name,
			Image:       c.Image,
			ImageID:     c.ImageID,
			Status:      c.Status,
			Health:      c.Health,
			Schedule:    c.Schedule,
			Running:     c.Running,
			Managed:     c.Managed,
			MonitorOnly: c.MonitorOnly,
			Approve:     c.Approve,
			Pinned:      c.Pinned,
			CanRollback: rollback[c.Name],
		}
		if e, ok := last[c.Name]; ok {
			row.LastStatus = string(e.Status)
			row.LastTime = e.Time
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) summary(containers []engine.ContainerView, history []state.Entry, pending []state.Pending) summary {
	sum := summary{Total: len(containers), Pending: len(pending)}
	for _, c := range containers {
		if c.Managed {
			sum.Managed++
		}
		if c.Running {
			sum.Running++
		}
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range history {
		if e.Time.Before(cutoff) {
			continue
		}
		switch e.Status {
		case state.StatusUpdated:
			sum.Updated24h++
		case state.StatusFailed:
			sum.Failed24h++
		case state.StatusRolledBack:
			sum.RolledBack24h++
		}
	}
	snap := s.svc.Metrics().Snapshot()
	sum.Runs = snap.Runs
	if !snap.LastRun.IsZero() {
		sum.LastRun = snap.LastRun
		sum.LastRunFor = time.Duration(snap.LastRunSeconds * float64(time.Second)).Round(time.Millisecond).String()
	}
	return sum
}

func historyRows(entries []state.Entry, limit int) []historyRow {
	rows := make([]historyRow, 0, len(entries))
	for _, e := range entries {
		if limit > 0 && len(rows) >= limit {
			break
		}
		rows = append(rows, historyRow{
			Time:      e.Time,
			Container: e.Container,
			Image:     e.Image,
			Status:    string(e.Status),
			Message:   e.Message,
			OldImage:  shortID(e.OldImageID),
			NewImage:  shortID(e.NewImageID),
		})
	}
	return rows
}

func historyRowsFor(entries []state.Entry, container, status string) []historyRow {
	filtered := make([]state.Entry, 0, len(entries))
	for _, e := range entries {
		if container != "" && !strings.Contains(e.Container, container) {
			continue
		}
		if status != "" && string(e.Status) != status {
			continue
		}
		filtered = append(filtered, e)
	}
	return historyRows(filtered, 0)
}

func filterRows(rows []containerRow, q string) []containerRow {
	q = strings.ToLower(q)
	out := make([]containerRow, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Name), q) || strings.Contains(strings.ToLower(r.Image), q) {
			out = append(out, r)
		}
	}
	return out
}

func buildSettings(cfg *config.Config) *settingsView {
	ui := cfg.API.UI
	s := &settingsView{
		Interval:       cfg.Interval.Duration.String(),
		Schedule:       cfg.Schedule,
		Scope:          cfg.Scope,
		StateFile:      cfg.StateFile,
		LogLevel:       cfg.LogLevel,
		LogFormat:      cfg.LogFormat,
		Concurrency:    cfg.Concurrency,
		KeepImages:     cfg.KeepImages,
		Cleanup:        cfg.Cleanup,
		Rollback:       cfg.Rollback,
		DryRun:         cfg.DryRun,
		LabelEnable:    cfg.LabelEnable,
		LabelPrefix:    cfg.LabelPrefix,
		MonitorOnly:    cfg.MonitorOnly,
		IncludeStopped: cfg.IncludeStopped,
		RemoveVolumes:  cfg.RemoveVolumes,
		StopTimeout:    cfg.StopTimeout.Duration.String(),
		HealthTimeout:  cfg.HealthTimeout.Duration.String(),
		Settle:         cfg.Settle.Duration.String(),
		APITokenSet:    cfg.API.Token != "",
		APIAddr:        cfg.API.Addr,
		UIEnabled:      ui.Enabled,
		UIAuth:         ui.AuthEnabled(),
		UIUser:         ui.Username,
	}
	if !ui.AuthEnabled() {
		s.UIUser = "—"
	} else if ui.Username == "" {
		s.UIUser = "admin"
	} else {
		s.UIUser = ui.Username
	}

	hosts := make([]string, 0, len(cfg.Registry.Auths))
	for h := range cfg.Registry.Auths {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		s.Registries = append(s.Registries, registryRow{Host: h, Username: cfg.Registry.Auths[h].Username})
	}

	for _, n := range cfg.Notifications {
		s.Notifications = append(s.Notifications, notifRow{
			Type:   n.Type,
			Target: redactURL(n.URL),
			Events: strings.Join(n.Events, ", "),
		})
	}

	names := make([]string, 0, len(cfg.Containers))
	for name := range cfg.Containers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ov := cfg.Containers[name]
		var parts []string
		if ov.MonitorOnly != nil {
			parts = append(parts, fmt.Sprintf("monitor_only=%t", *ov.MonitorOnly))
		}
		if ov.Approve != nil {
			parts = append(parts, fmt.Sprintf("approve=%t", *ov.Approve))
		}
		if ov.Schedule != "" {
			parts = append(parts, "schedule="+ov.Schedule)
		}
		if ov.Priority != nil {
			parts = append(parts, fmt.Sprintf("priority=%d", *ov.Priority))
		}
		if len(parts) == 0 {
			parts = append(parts, "configured")
		}
		s.Overrides = append(s.Overrides, overrideRow{Name: name, Summary: strings.Join(parts, " · ")})
	}
	return s
}

// --- helpers ---------------------------------------------------------------

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, v view) {
	t, ok := s.tmpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	if v.Version == "" {
		v.Version = version.Version
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if v.Status != 0 {
		w.WriteHeader(v.Status)
	}
	if err := t.ExecuteTemplate(w, "layout", v); err != nil {
		s.log.Error("render dashboard", "page", page, "error", err)
	}
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func statusClass(status string) string {
	switch status {
	case string(state.StatusUpdated):
		return "ok"
	case string(state.StatusFailed):
		return "err"
	case string(state.StatusRolledBack):
		return "warn"
	case string(state.StatusPending):
		return "info"
	default:
		return "muted"
	}
}

func healthClass(health string) string {
	switch health {
	case "healthy":
		return "ok"
	case "unhealthy":
		return "err"
	case "starting":
		return "warn"
	default:
		return "muted"
	}
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func shortID(id string) string {
	if id == "" {
		return ""
	}
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func redactURL(raw string) string {
	if raw == "" {
		return "—"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "•••"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

func safeNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// --- login rate limiting ---------------------------------------------------

type limiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int
	window   time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{attempts: map[string][]time.Time{}, max: max, window: window}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	kept := make([]time.Time, 0, len(l.attempts[key]))
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, now)
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}
