// Package notify delivers update reports to webhooks, Slack-compatible
// endpoints, or a generic JSON receiver.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/version"
)

// Result is the notifiable outcome for one container.
type Result struct {
	Container  string `json:"container"`
	Image      string `json:"image"`
	Status     string `json:"status"`
	OldImageID string `json:"old_image_id,omitempty"`
	NewImageID string `json:"new_image_id,omitempty"`
	Message    string `json:"message,omitempty"`
	RolledBack bool   `json:"rolled_back,omitempty"`
}

// Report is the payload sent to every sink.
type Report struct {
	Host       string    `json:"host,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Duration   string    `json:"duration"`
	DryRun     bool      `json:"dry_run"`
	Updated    int       `json:"updated"`
	Failed     int       `json:"failed"`
	RolledBack int       `json:"rolled_back"`
	UpToDate   int       `json:"up_to_date"`
	Pending    int       `json:"pending"`
	Results    []Result  `json:"results"`
}

// Summary renders a one-line human summary.
func (r Report) Summary() string {
	parts := []string{fmt.Sprintf("drydock: %d updated, %d failed", r.Updated, r.Failed)}
	if r.RolledBack > 0 {
		parts = append(parts, fmt.Sprintf("%d rolled back", r.RolledBack))
	}
	if r.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", r.Pending))
	}
	if r.DryRun {
		parts = append(parts, "dry run")
	}
	return strings.Join(parts, ", ")
}

// Text renders a multi-line human-readable report.
func (r Report) Text() string {
	var b strings.Builder
	b.WriteString(r.Summary())
	b.WriteString(" in ")
	b.WriteString(r.Duration)
	for _, res := range r.Results {
		fmt.Fprintf(&b, "\n  - %s (%s): %s", res.Container, res.Image, res.Status)
		if res.Message != "" {
			fmt.Fprintf(&b, " - %s", res.Message)
		}
	}
	return b.String()
}

// Dispatcher fans a report out to all configured sinks.
type Dispatcher struct {
	sinks []sink
	log   *slog.Logger
	http  *http.Client
}

type sink struct {
	cfg config.Notification
}

// New builds a dispatcher. Invalid sinks are skipped with a warning.
func New(cfgs []config.Notification, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	d := &Dispatcher{log: log, http: &http.Client{Timeout: 15 * time.Second}}
	for _, c := range cfgs {
		switch c.Type {
		case "webhook", "json", "slack":
			d.sinks = append(d.sinks, sink{cfg: c})
		default:
			log.Warn("ignoring unknown notification type", "type", c.Type)
		}
	}
	return d
}

// Enabled reports whether any sink is configured.
func (d *Dispatcher) Enabled() bool { return d != nil && len(d.sinks) > 0 }

// Send delivers the report. Failures are logged and counted, never fatal.
func (d *Dispatcher) Send(ctx context.Context, r Report) {
	if !d.Enabled() {
		return
	}
	for _, s := range d.sinks {
		if !s.wants(r) {
			continue
		}
		if err := d.sendOne(ctx, s.cfg, r); err != nil {
			d.log.Warn("notification failed", "type", s.cfg.Type, "error", err)
		}
	}
}

func (s sink) wants(r Report) bool {
	if len(s.cfg.Events) == 0 {
		return true
	}
	have := map[string]bool{"success": r.Failed == 0, "failure": r.Failed > 0}
	for _, e := range s.cfg.Events {
		switch strings.ToLower(strings.TrimSpace(e)) {
		case "update", "updated":
			if r.Updated > 0 {
				return true
			}
		case "rollback", "rolled_back":
			if r.RolledBack > 0 {
				return true
			}
		default:
			if have[strings.ToLower(e)] {
				return true
			}
		}
	}
	return false
}

func (d *Dispatcher) sendOne(ctx context.Context, cfg config.Notification, r Report) error {
	var payload any = r
	if cfg.Type == "slack" {
		payload = map[string]any{
			"text": r.Text(),
			"blocks": []map[string]any{
				{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": "*" + r.Summary() + "*"}},
				{"type": "section", "text": map[string]string{"type": "plain_text", "text": r.Text()}},
			},
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return nil
}
