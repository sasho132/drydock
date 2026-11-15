// Package state persists update history, rollback snapshots, and pending
// approvals. The store is a single JSON document written atomically, so a
// crash can never leave a half-written file behind.
//
// The file contains container config snapshots (including environment
// variables), so it is created with mode 0600.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sasho/drydock/internal/model"
)

// Status describes the outcome recorded for a container.
type Status string

const (
	StatusUpdated    Status = "updated"
	StatusUpToDate   Status = "up_to_date"
	StatusFailed     Status = "failed"
	StatusRolledBack Status = "rolled_back"
	StatusSkipped    Status = "skipped"
	StatusPending    Status = "pending"
)

// Entry is one immutable record in the update history.
type Entry struct {
	ID          string          `json:"id"`
	Time        time.Time       `json:"time"`
	Container   string          `json:"container"`
	ContainerID string          `json:"container_id"`
	Image       string          `json:"image"`
	OldImageID  string          `json:"old_image_id,omitempty"`
	NewImageID  string          `json:"new_image_id,omitempty"`
	OldDigest   string          `json:"old_digest,omitempty"`
	NewDigest   string          `json:"new_digest,omitempty"`
	Status      Status          `json:"status"`
	Message     string          `json:"message,omitempty"`
	RolledBack  bool            `json:"rolled_back,omitempty"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
	Snapshot    *model.Snapshot `json:"snapshot,omitempty"`
}

// Reasons a Pending entry was recorded. Only check/approval entries can be
// applied from the dashboard; dry-run and monitor-only never change anything.
const (
	PendingReasonCheck       = "check"
	PendingReasonApproval    = "approval"
	PendingReasonDryRun      = "dry-run"
	PendingReasonMonitorOnly = "monitor-only"
)

// Pending is a detected update awaiting manual approval.
type Pending struct {
	Container  string    `json:"container"`
	Image      string    `json:"image"`
	OldImageID string    `json:"old_image_id"`
	NewImageID string    `json:"new_image_id,omitempty"`
	NewDigest  string    `json:"new_digest,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	DetectedAt time.Time `json:"detected_at"`
}

// Actionable reports whether approving this entry can actually update the
// container. Entries from monitor-only or dry-run runs cannot.
func (p Pending) Actionable() bool {
	return p.Reason != PendingReasonDryRun && p.Reason != PendingReasonMonitorOnly
}

// Data is the on-disk document.
type Data struct {
	Version   int                 `json:"version"`
	UpdatedAt time.Time           `json:"updated_at"`
	History   []Entry             `json:"history"`
	Images    map[string][]string `json:"images"`
	Pending   map[string]Pending  `json:"pending"`
}

// Store is a concurrency-safe handle to the state file.
type Store struct {
	path string
	mu   sync.Mutex
	data Data
}

const (
	formatVersion = 1
	maxHistory    = 2000
)

// Open loads the store at path, creating parent directories if needed.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state file path is empty")
	}
	s := &Store{path: path, data: Data{
		Version: formatVersion,
		Images:  map[string][]string{},
		Pending: map[string]Pending{},
	}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("parse state file %q: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// Fresh install.
	default:
		return nil, fmt.Errorf("read state file %q: %w", path, err)
	}
	if s.data.Images == nil {
		s.data.Images = map[string][]string{}
	}
	if s.data.Pending == nil {
		s.data.Pending = map[string]Pending{}
	}
	s.data.Version = formatVersion
	return s, nil
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// Append records an entry and persists the store.
func (s *Store) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	s.data.History = append(s.data.History, e)
	if len(s.data.History) > maxHistory {
		s.data.History = s.data.History[len(s.data.History)-maxHistory:]
	}
	return s.saveLocked()
}

// History returns up to limit entries, newest first. A limit <= 0 returns all.
func (s *Store) History(limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.data.History))
	copy(out, s.data.History)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// LastSuccessful returns the most recent successful update for a container,
// which is the rollback target.
func (s *Store) LastSuccessful(container string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.data.History) - 1; i >= 0; i-- {
		e := s.data.History[i]
		if e.Container == container && e.Status == StatusUpdated && e.Snapshot != nil {
			return e, true
		}
	}
	return Entry{}, false
}

// TrackImage remembers that imageID was the running image for a container.
func (s *Store) TrackImage(container, imageID string) error {
	if imageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.Images[container]
	for _, existing := range list {
		if existing == imageID {
			return nil
		}
	}
	s.data.Images[container] = append(list, imageID)
	return s.saveLocked()
}

// ImagesToPrune returns image IDs beyond the newest keep entries for a
// container, oldest first.
func (s *Store) ImagesToPrune(container string, keep int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.Images[container]
	if keep < 0 {
		keep = 0
	}
	if len(list) <= keep {
		return nil
	}
	drop := list[:len(list)-keep]
	s.data.Images[container] = list[len(list)-keep:]
	return append([]string(nil), drop...)
}

// ForgetImage removes an image ID from a container's tracked history.
func (s *Store) ForgetImage(container, imageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.Images[container]
	out := list[:0]
	for _, id := range list {
		if id != imageID {
			out = append(out, id)
		}
	}
	s.data.Images[container] = out
}

// Save persists pending changes made by ImagesToPrune/ForgetImage.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// SetPending records a detected update awaiting approval.
func (s *Store) SetPending(p Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.DetectedAt.IsZero() {
		p.DetectedAt = time.Now().UTC()
	}
	s.data.Pending[p.Container] = p
	return s.saveLocked()
}

// ClearPending removes a pending approval after it has been applied or is no
// longer relevant.
func (s *Store) ClearPending(container string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Pending, container)
	return s.saveLocked()
}

// Pending returns all pending approvals sorted by container name.
func (s *Store) Pending() []Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Pending, 0, len(s.data.Pending))
	for _, p := range s.data.Pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out
}

func (s *Store) saveLocked() error {
	s.data.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".drydock-state-*")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
