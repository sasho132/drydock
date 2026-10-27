package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// JobKind identifies the work a background job performs.
type JobKind string

const (
	// JobCheck resolves digests without changing anything.
	JobCheck JobKind = "check"
	// JobUpdate pulls and applies available updates.
	JobUpdate JobKind = "update"
	// JobRollback restores a container to its previous image.
	JobRollback JobKind = "rollback"
)

// JobStatus is the lifecycle state of a background job.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
)

// ErrJobBusy is returned when a job is already queued or running. Serialising
// jobs keeps the UI predictable and avoids a pile-up of overlapping requests.
var ErrJobBusy = errors.New("another run is already in progress")

// Job is a snapshot of a background run. It is safe to serialise.
type Job struct {
	ID         string     `json:"id"`
	Kind       JobKind    `json:"kind"`
	Status     JobStatus  `json:"status"`
	Container  string     `json:"container,omitempty"`
	Options    Options    `json:"options,omitzero"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Report     *Report    `json:"report,omitempty"`
	Outcome    *Outcome   `json:"outcome,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Running reports whether the job has not finished yet.
func (j Job) Running() bool { return j.Status == JobQueued || j.Status == JobRunning }

// Duration returns how long the job has been running, or ran for.
func (j Job) Duration() time.Duration {
	end := time.Now()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(j.StartedAt)
}

// StartJob launches a background job and returns immediately. Only one job may
// be active at a time; ErrJobBusy is returned otherwise.
func (e *Engine) StartJob(kind JobKind, opts Options, container string) (Job, error) {
	switch kind {
	case JobCheck, JobUpdate:
	case JobRollback:
		if container == "" {
			return Job{}, errors.New("rollback requires a container")
		}
	default:
		return Job{}, fmt.Errorf("unknown job kind %q", kind)
	}

	e.jobsMu.Lock()
	e.initJobsLocked()
	if e.activeJobLocked() != nil {
		e.jobsMu.Unlock()
		return Job{}, ErrJobBusy
	}
	job := &Job{
		ID:        newJobID(),
		Kind:      kind,
		Status:    JobQueued,
		Container: container,
		Options:   opts,
		StartedAt: time.Now().UTC(),
	}
	e.jobs[job.ID] = job
	e.jobOrder = append(e.jobOrder, job.ID)
	snapshot := *job
	e.pruneJobsLocked(100)
	e.jobsMu.Unlock()

	go e.runJob(job.ID, kind, opts, container)
	return snapshot, nil
}

func (e *Engine) runJob(id string, kind JobKind, opts Options, container string) {
	e.updateJob(id, func(j *Job) { j.Status = JobRunning })

	ctx := context.Background()
	var (
		report  *Report
		outcome *Outcome
		err     error
	)
	switch kind {
	case JobCheck:
		o := opts
		o.CheckOnly = true
		report, err = e.Run(ctx, o)
	case JobUpdate:
		report, err = e.Run(ctx, opts)
	case JobRollback:
		var out Outcome
		out, err = e.Rollback(ctx, container)
		outcome = &out
	}

	finished := time.Now().UTC()
	e.updateJob(id, func(j *Job) {
		j.FinishedAt = &finished
		j.Report = report
		j.Outcome = outcome
		if err != nil {
			j.Status = JobFailed
			j.Error = err.Error()
			return
		}
		j.Status = JobSucceeded
	})
}

// Job returns a copy of a job by ID.
func (e *Engine) Job(id string) (Job, bool) {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Jobs returns up to limit recent jobs, newest first.
func (e *Engine) Jobs(limit int) []Job {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	out := make([]Job, 0, len(e.jobOrder))
	for i := len(e.jobOrder) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		if j, ok := e.jobs[e.jobOrder[i]]; ok {
			out = append(out, *j)
		}
	}
	return out
}

// ActiveJob returns the currently queued or running job, if any.
func (e *Engine) ActiveJob() (Job, bool) {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if j := e.activeJobLocked(); j != nil {
		return *j, true
	}
	return Job{}, false
}

func (e *Engine) initJobsLocked() {
	if e.jobs == nil {
		e.jobs = map[string]*Job{}
	}
}

func (e *Engine) activeJobLocked() *Job {
	for i := len(e.jobOrder) - 1; i >= 0; i-- {
		if j, ok := e.jobs[e.jobOrder[i]]; ok && j.Running() {
			return j
		}
	}
	return nil
}

func (e *Engine) updateJob(id string, fn func(*Job)) {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	if j, ok := e.jobs[id]; ok {
		fn(j)
	}
}

func (e *Engine) pruneJobsLocked(max int) {
	for len(e.jobOrder) > max {
		oldest := e.jobOrder[0]
		e.jobOrder = e.jobOrder[1:]
		delete(e.jobs, oldest)
	}
}

// JobBusy reports whether a job is currently active.
func (e *Engine) JobBusy() bool {
	e.jobsMu.Lock()
	defer e.jobsMu.Unlock()
	return e.activeJobLocked() != nil
}

// waitForJob is used in tests to block until a job finishes.
func (e *Engine) waitForJob(id string, timeout time.Duration) (Job, bool) {
	deadline := time.Now().Add(timeout)
	for {
		j, ok := e.Job(id)
		if !ok {
			return Job{}, false
		}
		if !j.Running() {
			return j, true
		}
		if time.Now().After(deadline) {
			return j, true
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
