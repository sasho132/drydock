package scheduler

import (
	"testing"
	"time"
)

func TestNextDuration(t *testing.T) {
	from := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := Next("6h", from, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := from.Add(6 * time.Hour); !got.Equal(want) {
		t.Errorf("Next(6h) = %s, want %s", got, want)
	}
}

func TestNextEmptyUsesInterval(t *testing.T) {
	from := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := Next("", from, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := from.Add(30 * time.Minute); !got.Equal(want) {
		t.Errorf("Next(\"\") = %s, want %s", got, want)
	}
}

func TestNextEveryDescriptor(t *testing.T) {
	from := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := Next("@every 15m", from, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := from.Add(15 * time.Minute); !got.Equal(want) {
		t.Errorf("Next(@every 15m) = %s, want %s", got, want)
	}
}

func TestNextCron(t *testing.T) {
	from := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := Next("0 4 * * *", from, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2025, 1, 2, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Next(0 4 * * *) = %s, want %s", got, want)
	}
}

func TestNextCronWithSeconds(t *testing.T) {
	from := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	got, err := Next("*/30 * * * * *", from, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := from.Add(30 * time.Second); !got.Equal(want) {
		t.Errorf("Next(30s cron) = %s, want %s", got, want)
	}
}

func TestValidateRejectsGarbage(t *testing.T) {
	if err := Validate("not a schedule"); err == nil {
		t.Fatal("expected an error")
	}
	if err := Validate(""); err != nil {
		t.Fatalf("empty schedule should be valid: %v", err)
	}
	if err := Validate("-5m"); err == nil {
		t.Fatal("negative duration should be rejected")
	}
}
