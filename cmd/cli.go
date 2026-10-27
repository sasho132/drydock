// Package cmd implements drydock's command-line interface.
package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/sasho/drydock/internal/api"
	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/docker"
	"github.com/sasho/drydock/internal/engine"
	"github.com/sasho/drydock/internal/logx"
	"github.com/sasho/drydock/internal/metrics"
	"github.com/sasho/drydock/internal/notify"
	"github.com/sasho/drydock/internal/registry"
	"github.com/sasho/drydock/internal/scheduler"
	"github.com/sasho/drydock/internal/state"
	"github.com/sasho/drydock/internal/version"
)

const usage = `drydock - keep Docker containers up to date, safely

Usage:
  drydock [command] [flags]

Commands:
  run          Run continuously, checking on the configured schedule (default)
  update       Run one update cycle and exit
  check        Report available updates without applying them
  containers   List containers and their resolved policy
  history      Show the update history
  rollback     Restore a container to the image it ran before its last update
  validate     Validate the configuration and exit
  version      Print version information

Common flags:
  --config PATH             Configuration file (default: ./drydock.yaml, /etc/drydock/config.yaml)
  --interval DURATION       Default check interval (default 6h)
  --schedule SPEC           Cron expression, e.g. "0 4 * * *" or "@every 6h"
  --state-file PATH         Where to store history and rollback snapshots
  --docker-host HOST        Docker daemon (default: $DOCKER_HOST or unix socket)
  --log-level LEVEL         debug, info, warn, error (default info)
  --log-format FORMAT       text or json (default text)
  --concurrency N           Containers updated in parallel (default 2)
  --stop-timeout DURATION   Graceful stop timeout (default 30s)
  --health-timeout DURATION How long to wait for a new container to become healthy
  --settle DURATION         How long a container without a healthcheck must stay up
  --rollback=BOOL           Auto-restore the previous container on failure (default true)
  --cleanup                 Remove previous images after a successful update
  --keep-images N           Previous images to retain for rollback (default 2)
  --monitor-only            Detect updates but never apply them
  --label-enable            Only manage containers labelled <prefix>.enable=true
  --include-stopped         Also consider stopped containers
  --dry-run                 Do everything except pull and recreate
  --api                     Enable the HTTP API, dashboard, and /metrics
  --api-addr ADDR           API listen address (default :8080)
  --api-token TOKEN         Require a bearer token on mutating API calls
  --ui=BOOL                 Serve the web dashboard (default true)
  --ui-username NAME        Dashboard username (default "admin")
  --ui-password PASS        Dashboard password; enables actions. Prefer
                            DRYDOCK_UI_PASSWORD so it stays out of the process list
  --only NAME               Restrict to a container (repeatable)
  --json                    Machine-readable output for one-shot commands

Examples:
  drydock run --interval 30m
  drydock check --json
  drydock update --only web --force
  drydock rollback web
  drydock history --limit 20
`

// Main is the process entry point. It returns the exit code.
func Main() int {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(usage)
			return 0
		}
	}

	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "run", "daemon":
		return runDaemon(args)
	case "update", "once":
		return runOnce(args, false)
	case "check":
		return runOnce(args, true)
	case "containers", "ls":
		return runContainers(args)
	case "history":
		return runHistory(args)
	case "rollback":
		return runRollback(args)
	case "validate":
		return runValidate(args)
	case "version":
		fmt.Printf("drydock %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		fmt.Print(usage)
		return 2
	}
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type common struct {
	fs     *flag.FlagSet
	visits map[string]bool

	configPath string

	interval    time.Duration
	schedule    string
	logLevel    string
	logFormat   string
	stateFile   string
	dockerHost  string
	scope       string
	concurrency int
	keepImages  int

	dryRun         bool
	monitorOnly    bool
	labelEnable    bool
	cleanup        bool
	rollback       bool
	includeStopped bool

	stopTimeout   time.Duration
	healthTimeout time.Duration
	settle        time.Duration

	apiEnabled bool
	apiAddr    string
	apiToken   string
	uiEnabled  bool
	uiUsername string
	uiPassword string

	only       multi
	force      bool
	jsonOut    bool
	limit      int
	exitCode   bool
	timeout    time.Duration
	initialRun bool
}

func newCommon(name string, out io.Writer) *common {
	c := &common{fs: flag.NewFlagSet(name, flag.ContinueOnError), visits: map[string]bool{}}
	c.fs.SetOutput(out)

	c.fs.StringVar(&c.configPath, "config", "", "configuration file")
	c.fs.DurationVar(&c.interval, "interval", 0, "default check interval")
	c.fs.StringVar(&c.schedule, "schedule", "", "cron expression or @every duration")
	c.fs.StringVar(&c.logLevel, "log-level", "", "debug, info, warn, error")
	c.fs.StringVar(&c.logFormat, "log-format", "", "text or json")
	c.fs.StringVar(&c.stateFile, "state-file", "", "state file path")
	c.fs.StringVar(&c.dockerHost, "docker-host", "", "docker daemon host")
	c.fs.StringVar(&c.scope, "scope", "", "only manage containers with this scope")
	c.fs.IntVar(&c.concurrency, "concurrency", 0, "containers updated in parallel")
	c.fs.IntVar(&c.keepImages, "keep-images", -1, "previous images to retain")

	c.fs.BoolVar(&c.dryRun, "dry-run", false, "report without applying")
	c.fs.BoolVar(&c.monitorOnly, "monitor-only", false, "detect but do not apply updates")
	c.fs.BoolVar(&c.labelEnable, "label-enable", false, "require the enable label")
	c.fs.BoolVar(&c.cleanup, "cleanup", false, "remove previous images")
	c.fs.BoolVar(&c.rollback, "rollback", true, "auto-rollback failed updates")
	c.fs.BoolVar(&c.includeStopped, "include-stopped", false, "also consider stopped containers")

	c.fs.DurationVar(&c.stopTimeout, "stop-timeout", 0, "graceful stop timeout")
	c.fs.DurationVar(&c.healthTimeout, "health-timeout", 0, "health verification timeout")
	c.fs.DurationVar(&c.settle, "settle", 0, "stability window without a healthcheck")

	c.fs.BoolVar(&c.apiEnabled, "api", false, "enable the HTTP API and dashboard")
	c.fs.StringVar(&c.apiAddr, "api-addr", "", "API listen address")
	c.fs.StringVar(&c.apiToken, "api-token", "", "API bearer token")
	c.fs.BoolVar(&c.uiEnabled, "ui", true, "serve the web dashboard")
	c.fs.StringVar(&c.uiUsername, "ui-username", "", "dashboard username")
	c.fs.StringVar(&c.uiPassword, "ui-password", "", "dashboard password (prefer DRYDOCK_UI_PASSWORD)")

	c.fs.Var(&c.only, "only", "restrict to a container (repeatable)")
	c.fs.BoolVar(&c.force, "force", false, "apply updates that require approval")
	c.fs.BoolVar(&c.jsonOut, "json", false, "JSON output")
	c.fs.IntVar(&c.limit, "limit", 50, "number of history entries")
	c.fs.BoolVar(&c.exitCode, "exit-code", false, "check: exit 2 when updates are available")
	c.fs.DurationVar(&c.timeout, "timeout", 0, "overall run timeout (0 = none)")
	c.fs.BoolVar(&c.initialRun, "initial-run", true, "run once at startup")
	return c
}

func (c *common) parse(args []string) error {
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	c.fs.Visit(func(f *flag.Flag) { c.visits[f.Name] = true })
	return nil
}

func (c *common) load() (*config.Config, error) {
	path := c.configPath
	if path == "" {
		path = config.Discover()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.ApplyEnv(nil); err != nil {
		return nil, err
	}
	c.apply(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Schedule != "" {
		if err := scheduler.Validate(cfg.Schedule); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func (c *common) apply(cfg *config.Config) {
	set := func(name string) bool { return c.visits[name] }
	if set("interval") {
		cfg.Interval = config.Duration{Duration: c.interval}
	}
	if set("schedule") {
		cfg.Schedule = c.schedule
	}
	if set("log-level") {
		cfg.LogLevel = c.logLevel
	}
	if set("log-format") {
		cfg.LogFormat = c.logFormat
	}
	if set("state-file") {
		cfg.StateFile = c.stateFile
	}
	if set("docker-host") {
		cfg.DockerHost = c.dockerHost
	}
	if set("scope") {
		cfg.Scope = c.scope
	}
	if set("concurrency") {
		cfg.Concurrency = c.concurrency
	}
	if set("keep-images") {
		cfg.KeepImages = c.keepImages
	}
	if set("dry-run") {
		cfg.DryRun = c.dryRun
	}
	if set("monitor-only") {
		cfg.MonitorOnly = c.monitorOnly
	}
	if set("label-enable") {
		cfg.LabelEnable = c.labelEnable
	}
	if set("cleanup") {
		cfg.Cleanup = c.cleanup
	}
	if set("rollback") {
		cfg.Rollback = c.rollback
	}
	if set("include-stopped") {
		cfg.IncludeStopped = c.includeStopped
	}
	if set("stop-timeout") {
		cfg.StopTimeout = config.Duration{Duration: c.stopTimeout}
	}
	if set("health-timeout") {
		cfg.HealthTimeout = config.Duration{Duration: c.healthTimeout}
	}
	if set("settle") {
		cfg.Settle = config.Duration{Duration: c.settle}
	}
	if set("api") {
		cfg.API.Enabled = c.apiEnabled
	}
	if set("api-addr") {
		cfg.API.Addr = c.apiAddr
	}
	if set("api-token") {
		cfg.API.Token = c.apiToken
	}
	if set("ui") {
		cfg.API.UI.Enabled = c.uiEnabled
	}
	if set("ui-username") {
		cfg.API.UI.Username = c.uiUsername
	}
	if set("ui-password") {
		cfg.API.UI.Password = c.uiPassword
	}
}

func (c *common) options() engine.Options {
	return engine.Options{DryRun: c.dryRun, Force: c.force, Only: c.only}
}

type deps struct {
	cfg     *config.Config
	docker  *docker.Runtime
	store   *state.Store
	engine  *engine.Engine
	metrics *metrics.Registry
}

// runDaemon starts the long-running scheduler and, optionally, the HTTP API.
func runDaemon(args []string) int {
	c := newCommon("run", os.Stderr)
	if err := c.parse(args); err != nil {
		return 2
	}
	cfg, err := c.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	log := logx.New(cfg.LogLevel, cfg.LogFormat, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := setup(ctx, cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	defer d.close()

	if cfg.API.Enabled {
		ln, err := api.Listen(cfg.API.Addr)
		if err != nil {
			log.Error("could not start api", "error", err)
			return 1
		}
		srv := api.New(d.engine, cfg, log)
		go func() {
			if err := srv.ServeListener(ctx, ln); err != nil {
				log.Error("api stopped", "error", err)
			}
		}()
	}

	log.Info("drydock started",
		"version", version.Version,
		"interval", cfg.Interval.Duration,
		"schedule", cfg.Schedule,
		"state_file", cfg.StateFile,
		"concurrency", cfg.Concurrency,
		"rollback", cfg.Rollback,
		"dry_run", cfg.DryRun,
		"api", cfg.API.Enabled,
	)

	sched := scheduler.New(d.engine, cfg, log)
	sched.Immediate = c.initialRun
	sched.Only = c.only
	sched.Force = c.force
	if err := sched.Run(ctx); err != nil {
		log.Error("scheduler stopped with error", "error", err)
		return 1
	}
	log.Info("drydock stopped")
	return 0
}

func runOnce(args []string, checkOnly bool) int {
	name := "update"
	if checkOnly {
		name = "check"
	}
	c := newCommon(name, os.Stderr)
	if err := c.parse(args); err != nil {
		return 2
	}
	cfg, err := c.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	log := logx.New(cfg.LogLevel, cfg.LogFormat, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	d, err := setup(ctx, cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	defer d.close()

	opts := c.options()
	opts.CheckOnly = checkOnly
	report, err := d.engine.Run(ctx, opts)
	if err != nil {
		log.Error("run failed", "error", err)
		return 1
	}
	if err := printReport(os.Stdout, report, c.jsonOut); err != nil {
		log.Error("could not write report", "error", err)
		return 1
	}

	counts := report.Counts()
	if checkOnly && c.exitCode && counts[state.StatusPending] > 0 {
		return 2
	}
	if counts[state.StatusFailed] > 0 || counts[state.StatusRolledBack] > 0 {
		return 1
	}
	return 0
}

func runContainers(args []string) int {
	c := newCommon("containers", os.Stderr)
	if err := c.parse(args); err != nil {
		return 2
	}
	cfg, err := c.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	log := logx.New(cfg.LogLevel, cfg.LogFormat, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := setup(ctx, cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	defer d.close()

	views, err := d.engine.Containers(ctx)
	if err != nil {
		log.Error("could not list containers", "error", err)
		return 1
	}
	if c.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"containers": views}); err != nil {
			return 1
		}
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tSTATUS\tHEALTH\tMANAGED\tSCHEDULE")
	for _, v := range views {
		managed := "no"
		if v.Managed {
			managed = "yes"
		}
		sched := v.Schedule
		if sched == "" {
			sched = cfg.Interval.Duration.String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, truncate(v.Image, 48), v.Status, v.Health, managed, sched)
	}
	tw.Flush()
	return 0
}

func runHistory(args []string) int {
	c := newCommon("history", os.Stderr)
	if err := c.parse(args); err != nil {
		return 2
	}
	cfg, err := c.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	store, err := state.Open(cfg.StateFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open state:", err)
		return 1
	}
	entries := store.History(c.limit)

	if c.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"history": entries}); err != nil {
			return 1
		}
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tCONTAINER\tSTATUS\tIMAGE\tDETAIL")
	for _, e := range entries {
		detail := e.Message
		if e.OldImageID != "" && e.NewImageID != "" && e.OldImageID != e.NewImageID {
			detail = short(e.OldImageID) + " -> " + short(e.NewImageID) + " " + detail
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			e.Time.Local().Format("2006-01-02 15:04:05"), e.Container, e.Status, truncate(e.Image, 40), truncate(detail, 80))
	}
	tw.Flush()
	return 0
}

func runRollback(args []string) int {
	c := newCommon("rollback", os.Stderr)
	// Accept both "rollback <name> [flags]" and "rollback [flags] <name>".
	// The standard flag package stops at the first positional argument.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := c.parse(args); err != nil {
		return 2
	}
	if name == "" {
		name = c.fs.Arg(0)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: drydock rollback <container> [flags]")
		return 2
	}
	cfg, err := c.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 2
	}
	log := logx.New(cfg.LogLevel, cfg.LogFormat, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := setup(ctx, cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err)
		return 1
	}
	defer d.close()

	outcome, err := d.engine.Rollback(ctx, name)
	if c.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"outcome": outcome, "error": errString(err)})
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "rollback failed:", err)
	} else {
		fmt.Printf("rolled back %s to %s\n", outcome.Container, short(outcome.NewImageID))
	}
	if err != nil {
		return 1
	}
	return 0
}

func runValidate(args []string) int {
	c := newCommon("validate", os.Stderr)
	if err := c.parse(args); err != nil {
		return 2
	}
	cfg, err := c.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid:", err)
		return 1
	}
	fmt.Printf("configuration valid: interval=%s schedule=%q state=%s concurrency=%d\n",
		cfg.Interval.Duration, cfg.Schedule, cfg.StateFile, cfg.Concurrency)
	return 0
}

// setup builds the runtime, state store, registry client, and engine.
func setup(ctx context.Context, cfg *config.Config, log *slog.Logger) (*deps, error) {
	rt, err := docker.New(cfg.DockerHost, log)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rt.Ping(pingCtx); err != nil {
		rt.Close()
		return nil, err
	}

	store, err := state.Open(cfg.StateFile)
	if err != nil {
		rt.Close()
		return nil, err
	}
	reg := registry.New(cfg.Registry, log)
	m := metrics.New()
	m.SetBuildInfo(version.Version, version.Commit)
	dispatcher := notify.New(cfg.Notifications, log)
	eng := engine.New(cfg, rt, reg, store, dispatcher, m, log)

	return &deps{cfg: cfg, docker: rt, store: store, engine: eng, metrics: m}, nil
}

func (d *deps) close() {
	if d.docker != nil {
		_ = d.docker.Close()
	}
}

func printReport(w io.Writer, r *engine.Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	counts := r.Counts()
	fmt.Fprintf(w, "%s: %d checked, %d updated, %d failed, %d rolled back, %d up to date, %d pending (%s)\n",
		r.StartedAt.Local().Format("2006-01-02 15:04:05"),
		len(r.Outcomes),
		counts[state.StatusUpdated],
		counts[state.StatusFailed],
		counts[state.StatusRolledBack],
		counts[state.StatusUpToDate],
		counts[state.StatusPending],
		r.Duration.Round(time.Millisecond),
	)
	if r.DryRun {
		fmt.Fprintln(w, "(dry run: nothing was changed)")
	}
	for _, o := range r.Outcomes {
		line := fmt.Sprintf("  %-28s %-16s", truncate(o.Container, 28), o.Status)
		if o.OldImageID != "" && o.NewImageID != "" && o.OldImageID != o.NewImageID {
			line += fmt.Sprintf(" %s -> %s", short(o.OldImageID), short(o.NewImageID))
		}
		if o.Message != "" {
			line += "  (" + o.Message + ")"
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
