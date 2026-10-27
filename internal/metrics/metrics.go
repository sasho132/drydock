// Package metrics exposes drydock's counters in the Prometheus text format
// without pulling in the full Prometheus client dependency.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Registry holds counters and gauges. All methods are safe for concurrent use.
type Registry struct {
	mu sync.Mutex

	scans   map[string]uint64 // label: result
	updates map[string]uint64 // label: result
	notify  map[string]uint64 // label: result

	rollbacks   uint64
	containers  int
	pending     int
	runs        uint64
	lastRun     time.Time
	lastSeconds float64

	buildInfo map[string]string
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		scans:     map[string]uint64{},
		updates:   map[string]uint64{},
		notify:    map[string]uint64{},
		buildInfo: map[string]string{},
	}
}

// SetBuildInfo publishes the version/commit as a labelled gauge.
func (r *Registry) SetBuildInfo(version, commit string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buildInfo["version"] = version
	r.buildInfo["commit"] = commit
}

// IncScan counts a container whose staleness was evaluated.
func (r *Registry) IncScan(result string) { r.inc(r.scans, result) }

// IncUpdate counts a container update by result.
func (r *Registry) IncUpdate(result string) { r.inc(r.updates, result) }

// IncNotification counts a notification delivery by result.
func (r *Registry) IncNotification(result string) { r.inc(r.notify, result) }

// IncRollback counts an automatic or manual rollback.
func (r *Registry) IncRollback() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollbacks++
}

// SetContainers records how many containers were considered in the last run.
func (r *Registry) SetContainers(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers = n
}

// SetPending records how many updates await approval.
func (r *Registry) SetPending(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = n
}

// ObserveRun records a completed update cycle.
func (r *Registry) ObserveRun(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++
	r.lastRun = time.Now().UTC()
	r.lastSeconds = d.Seconds()
}

func (r *Registry) inc(m map[string]uint64, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m[label]++
}

// WritePrometheus renders all metrics in the Prometheus exposition format.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintf(w, "# HELP drydock_build_info Build information.\n")
	fmt.Fprintf(w, "# TYPE drydock_build_info gauge\n")
	fmt.Fprintf(w, "drydock_build_info{version=%q,commit=%q} 1\n",
		r.buildInfo["version"], r.buildInfo["commit"])

	counter(w, "drydock_scans_total", "Containers whose image was checked.", r.scans)
	counter(w, "drydock_updates_total", "Container updates by result.", r.updates)
	counter(w, "drydock_notifications_total", "Notification deliveries by result.", r.notify)

	fmt.Fprintf(w, "# HELP drydock_rollbacks_total Automatic and manual rollbacks performed.\n")
	fmt.Fprintf(w, "# TYPE drydock_rollbacks_total counter\n")
	fmt.Fprintf(w, "drydock_rollbacks_total %d\n", r.rollbacks)

	fmt.Fprintf(w, "# HELP drydock_runs_total Completed update cycles.\n")
	fmt.Fprintf(w, "# TYPE drydock_runs_total counter\n")
	fmt.Fprintf(w, "drydock_runs_total %d\n", r.runs)

	fmt.Fprintf(w, "# HELP drydock_last_run_timestamp_seconds Unix time of the last completed cycle.\n")
	fmt.Fprintf(w, "# TYPE drydock_last_run_timestamp_seconds gauge\n")
	fmt.Fprintf(w, "drydock_last_run_timestamp_seconds %d\n", r.lastRun.Unix())

	fmt.Fprintf(w, "# HELP drydock_last_run_duration_seconds Duration of the last cycle.\n")
	fmt.Fprintf(w, "# TYPE drydock_last_run_duration_seconds gauge\n")
	fmt.Fprintf(w, "drydock_last_run_duration_seconds %g\n", r.lastSeconds)

	fmt.Fprintf(w, "# HELP drydock_containers_managed Number of containers in the last cycle.\n")
	fmt.Fprintf(w, "# TYPE drydock_containers_managed gauge\n")
	fmt.Fprintf(w, "drydock_containers_managed %d\n", r.containers)

	fmt.Fprintf(w, "# HELP drydock_pending_updates Updates awaiting approval.\n")
	fmt.Fprintf(w, "# TYPE drydock_pending_updates gauge\n")
	fmt.Fprintf(w, "drydock_pending_updates %d\n", r.pending)
}

func counter(w io.Writer, name, help string, m map[string]uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{result=%q} %d\n", name, k, m[k])
	}
}

// Snapshot is a point-in-time copy of all counters, suitable for rendering in
// the dashboard without holding the registry lock.
type Snapshot struct {
	BuildInfo      map[string]string
	Scans          map[string]uint64
	Updates        map[string]uint64
	Notifications  map[string]uint64
	Rollbacks      uint64
	Containers     int
	Pending        int
	Runs           uint64
	LastRun        time.Time
	LastRunSeconds float64
}

// Snapshot returns an independent copy of the current metrics.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{
		BuildInfo:      cloneMap(r.buildInfo),
		Scans:          cloneMap(r.scans),
		Updates:        cloneMap(r.updates),
		Notifications:  cloneMap(r.notify),
		Rollbacks:      r.rollbacks,
		Containers:     r.containers,
		Pending:        r.pending,
		Runs:           r.runs,
		LastRun:        r.lastRun,
		LastRunSeconds: r.lastSeconds,
	}
}

func cloneMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
