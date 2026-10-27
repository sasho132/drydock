// Package api exposes a small JSON HTTP API plus health and Prometheus
// endpoints. It is entirely optional; drydock works fine without it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/engine"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/state"
	"github.com/sasho/drydock/internal/ui"
	"github.com/sasho/drydock/internal/version"
)

// Service is the subset of the engine the API needs.
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

// Server serves the drydock HTTP API.
type Server struct {
	engine  Service
	metrics *metrics.Registry
	token   string
	log     *slog.Logger
	mux     *http.ServeMux
	started time.Time
}

// New builds a server. cfg.API.Token, when non-empty, is required on mutating
// API routes. The embedded dashboard is mounted at "/" when enabled.
func New(eng Service, cfg *config.Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		engine:  eng,
		metrics: eng.Metrics(),
		token:   cfg.API.Token,
		log:     log,
		mux:     http.NewServeMux(),
		started: time.Now(),
	}
	s.routes()

	if cfg.API.UI.Enabled {
		// The dashboard needs the job API. Production always satisfies this;
		// lightweight test doubles may not, so fail soft rather than panicking.
		if uiSvc, ok := eng.(ui.Service); ok {
			s.mux.Handle("/", ui.New(uiSvc, cfg, log).Handler())
		} else {
			log.Warn("dashboard enabled but the engine does not provide the UI service")
		}
	}
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	return s.logRequests(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, "ok\n")
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.engine.Ready(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "docker daemon unavailable: "+err.Error())
			return
		}
		writeText(w, http.StatusOK, "ready\n")
	})
	s.mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.metrics.WritePrometheus(w)
	})

	s.mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"version": version.Version,
			"commit":  version.Commit,
			"build":   version.Date,
			"uptime":  time.Since(s.started).Round(time.Second).String(),
		})
	})

	s.mux.HandleFunc("GET /v1/containers", func(w http.ResponseWriter, r *http.Request) {
		views, err := s.engine.Containers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"containers": views})
	})

	s.mux.HandleFunc("GET /v1/history", func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &limit); err != nil {
				writeError(w, http.StatusBadRequest, "invalid limit")
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"history": s.engine.History(limit)})
	})

	s.mux.HandleFunc("GET /v1/pending", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"pending": s.engine.Pending()})
	})

	s.mux.HandleFunc("POST /v1/jobs", s.authorized(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind       string   `json:"kind"`
			Container  string   `json:"container"`
			Containers []string `json:"containers"`
			DryRun     bool     `json:"dry_run"`
			Force      bool     `json:"force"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}
		kind := engine.JobKind(body.Kind)
		if kind == "" {
			kind = engine.JobUpdate
		}
		only := body.Containers
		if len(only) == 0 && body.Container != "" {
			only = []string{body.Container}
		}
		job, err := s.engine.StartJob(kind, engine.Options{Only: only, DryRun: body.DryRun, Force: body.Force}, body.Container)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, engine.ErrJobBusy) {
				code = http.StatusConflict
			}
			writeError(w, code, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}))

	s.mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": s.engine.Jobs(50)})
	})

	s.mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, ok := s.engine.Job(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	})

	s.mux.HandleFunc("POST /v1/check", s.authorized(func(w http.ResponseWriter, r *http.Request) {
		report, err := s.engine.Run(r.Context(), engine.Options{CheckOnly: true, Only: queryOnly(r)})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, report)
	}))

	s.mux.HandleFunc("POST /v1/update", s.authorized(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Containers []string `json:"containers"`
			DryRun     bool     `json:"dry_run"`
			Force      bool     `json:"force"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
				return
			}
		}
		only := body.Containers
		if len(only) == 0 {
			only = queryOnly(r)
		}
		report, err := s.engine.Run(r.Context(), engine.Options{Only: only, DryRun: body.DryRun, Force: body.Force})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, report)
	}))

	s.mux.HandleFunc("POST /v1/rollback", s.authorized(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Container string `json:"container"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if body.Container == "" {
			writeError(w, http.StatusBadRequest, "container is required")
			return
		}
		outcome, err := s.engine.Rollback(r.Context(), body.Container)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"outcome": outcome, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, outcome)
	}))
}

func (s *Server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			auth := r.Header.Get("Authorization")
			tok, ok := strings.CutPrefix(auth, "Bearer ")
			if !ok || tok != s.token {
				writeError(w, http.StatusUnauthorized, "invalid or missing bearer token")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Listen binds the API address so a bad address fails at startup rather than
// silently in a goroutine.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := Listen(addr)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

// ServeListener serves on an already-bound listener.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		WriteTimeout:      15 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("api listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func queryOnly(r *http.Request) []string {
	var out []string
	for _, v := range r.URL.Query()["container"] {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeText(w http.ResponseWriter, code int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(s))
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
