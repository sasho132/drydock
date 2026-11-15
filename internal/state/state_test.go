package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sasho/drydock/internal/model"
)

func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Entry{Container: "web", Status: StatusUpdated, NewImageID: "sha256:new"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPending(Pending{Container: "web", Image: "app:2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TrackImage("web", "sha256:old"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.History(0)); got != 1 {
		t.Fatalf("history length = %d", got)
	}
	if got := len(reloaded.Pending()); got != 1 {
		t.Fatalf("pending length = %d", got)
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %o, want 600", st.Mode().Perm())
	}
}

func TestLastSuccessfulReturnsSnapshot(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	snap := &model.Snapshot{ImageID: "sha256:previous"}
	s.Append(Entry{Time: older, Container: "web", Status: StatusUpdated, Snapshot: snap})
	s.Append(Entry{Container: "web", Status: StatusUpToDate})

	entry, ok := s.LastSuccessful("web")
	if !ok {
		t.Fatal("expected a rollback target")
	}
	if entry.Snapshot == nil || entry.Snapshot.ImageID != "sha256:previous" {
		t.Fatalf("unexpected snapshot %+v", entry.Snapshot)
	}
	if _, ok := s.LastSuccessful("other"); ok {
		t.Error("unexpected rollback target for unknown container")
	}
}

func TestImagesToPruneKeepsNewest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sha256:1", "sha256:2", "sha256:3"} {
		s.TrackImage("web", id)
	}
	drop := s.ImagesToPrune("web", 2)
	if len(drop) != 1 || drop[0] != "sha256:1" {
		t.Fatalf("drop = %v", drop)
	}
	s.ForgetImage("web", "sha256:1")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if drop := s.ImagesToPrune("web", 2); len(drop) != 0 {
		t.Fatalf("expected nothing else to prune, got %v", drop)
	}
}

func TestPendingLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetPending(Pending{Container: "a"})
	s.SetPending(Pending{Container: "b"})
	if got := len(s.Pending()); got != 2 {
		t.Fatalf("pending = %d", got)
	}
	s.ClearPending("a")
	if got := len(s.Pending()); got != 1 {
		t.Fatalf("pending after clear = %d", got)
	}
}

func TestPendingActionable(t *testing.T) {
	cases := map[string]bool{
		"":                       true,
		PendingReasonCheck:       true,
		PendingReasonApproval:    true,
		PendingReasonDryRun:      false,
		PendingReasonMonitorOnly: false,
	}
	for reason, want := range cases {
		if got := (Pending{Reason: reason}).Actionable(); got != want {
			t.Errorf("reason %q actionable = %t, want %t", reason, got, want)
		}
	}
}
