// Package scheduler decides when containers are checked. A global interval or
// cron expression provides the default; any container can override it with a
// label, so a frequently changing service can be checked hourly while a
// database is only considered weekly.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/engine"
	"github.com/sasho/drydock/internal/policy"
)

var parser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Next computes the next run time for a schedule specification. Supported
// forms:
//
//	""              -> now + interval
//	"6h", "90m"     -> now + duration
//	"@every 30m"    -> cron descriptor
//	"0 4 * * *"     -> 5-field cron (also 6-field with seconds)
func Next(spec string, from time.Time, interval time.Duration) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		return from.Add(interval), nil
	}
	if d, err := time.ParseDuration(spec); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("duration %q must be positive", spec)
		}
		return from.Add(d), nil
	}
	sched, err := parser.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	return sched.Next(from), nil
}

// Validate reports whether spec is a usable schedule.
func Validate(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	_, err := Next(spec, time.Now(), time.Hour)
	return err
}

// Scheduler drives periodic update cycles.
type Scheduler struct {
	engine *engine.Engine
	cfg    *config.Config
	log    *slog.Logger

	next map[string]time.Time
	// Immediate runs every container as soon as it is discovered. Set to false
	// to wait for its first scheduled slot instead.
	Immediate bool
	// Refresh caps how long the scheduler sleeps while idle, so containers
	// created after startup are discovered promptly.
	Refresh time.Duration
	// Only restricts scheduled runs to matching container names or IDs.
	Only []string
	// Force applies updates that would otherwise require approval.
	Force bool
}

// New creates a scheduler.
func New(eng *engine.Engine, cfg *config.Config, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		engine:    eng,
		cfg:       cfg,
		log:       log,
		next:      map[string]time.Time{},
		Immediate: true,
		Refresh:   time.Minute,
	}
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		policies, _, err := s.engine.Managed(ctx)
		if err != nil {
			s.log.Error("could not list containers; retrying", "error", err)
			if !sleep(ctx, 30*time.Second) {
				return nil
			}
			continue
		}

		policies = filterOnly(policies, s.Only)

		now := time.Now()
		seen := make(map[string]bool, len(policies))
		var (
			due      []string
			earliest time.Time
		)

		for _, p := range policies {
			name := p.Container.Name()
			seen[name] = true

			next, known := s.next[name]
			if !known {
				if s.Immediate {
					next = now
				} else {
					next = s.scheduleNext(name, p.Schedule, now)
				}
				s.next[name] = next
			}

			if !next.After(now) {
				due = append(due, name)
				s.next[name] = s.scheduleNext(name, p.Schedule, now)
			}
			if n := s.next[name]; earliest.IsZero() || n.Before(earliest) {
				earliest = n
			}
		}
		for name := range s.next {
			if !seen[name] {
				delete(s.next, name)
			}
		}

		if len(due) > 0 {
			s.log.Info("starting scheduled run", "containers", due)
			report, err := s.engine.Run(ctx, engine.Options{Only: due, Force: s.Force})
			if err != nil {
				s.log.Error("scheduled run failed", "error", err)
			} else {
				counts := report.Counts()
				s.log.Info("scheduled run finished",
					"duration", report.Duration.Round(time.Millisecond),
					"updated", counts["updated"],
					"failed", counts["failed"],
					"rolled_back", counts["rolled_back"],
					"up_to_date", counts["up_to_date"])
			}
			continue
		}

		wait := time.Until(earliest)
		if s.Refresh > 0 && wait > s.Refresh {
			wait = s.Refresh
		}
		if wait < time.Second {
			wait = time.Second
		}
		s.log.Debug("scheduler idle", "sleep", wait.Round(time.Second), "containers", len(policies))
		if !sleep(ctx, wait) {
			return nil
		}
	}
}

func (s *Scheduler) scheduleNext(name, spec string, from time.Time) time.Time {
	next, err := Next(spec, from, s.cfg.Interval.Duration)
	if err != nil {
		s.log.Warn("invalid schedule, falling back to interval",
			"container", name, "schedule", spec, "fallback", s.cfg.Interval.Duration, "error", err)
		return from.Add(s.cfg.Interval.Duration)
	}
	return next
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// filterOnly keeps policies whose name or ID matches one of the filters.
func filterOnly(policies []policy.Policy, only []string) []policy.Policy {
	if len(only) == 0 {
		return policies
	}
	out := policies[:0]
	for _, p := range policies {
		name, id := p.Container.Name(), p.Container.ID()
		for _, want := range only {
			want = strings.TrimSpace(want)
			if want == "" {
				continue
			}
			if want == name || want == id || strings.HasPrefix(name, want) || strings.HasPrefix(id, want) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
