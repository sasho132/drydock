// Package docker wraps the Docker Engine API with the operations drydock needs:
// inspect, pull, and above all a rollback-capable container recreation.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sasho/drydock/internal/model"
	"github.com/sasho/drydock/internal/version"
)

// ErrNotFound is returned when a container or image does not exist.
var ErrNotFound = errors.New("not found")

// ErrImageChanged indicates the tag moved between pull and create.
var ErrImageChanged = errors.New("image changed during update")

// Runtime is a thin, testable facade over the Docker Engine API.
type Runtime struct {
	cli *client.Client
	log *slog.Logger
}

// New connects to the Docker daemon. host may be empty to use DOCKER_HOST or
// the platform default socket.
func New(host string, log *slog.Logger) (*Runtime, error) {
	if log == nil {
		log = slog.Default()
	}
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation(), client.WithUserAgent(version.UserAgent())}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Runtime{cli: cli, log: log}, nil
}

// Close releases idle connections.
func (r *Runtime) Close() error { return r.cli.Close() }

// Ping verifies that the daemon is reachable.
func (r *Runtime) Ping(ctx context.Context) error {
	if _, err := r.cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return fmt.Errorf("ping docker daemon: %w", err)
	}
	return nil
}

// List returns all containers (running and stopped) with their image details
// resolved. Containers whose image has been deleted are still returned, with a
// zero-value image inspect result.
func (r *Runtime) List(ctx context.Context) ([]*model.Container, error) {
	res, err := r.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]*model.Container, 0, len(res.Items))
	for _, item := range res.Items {
		c, err := r.Inspect(ctx, item.ID)
		if err != nil {
			r.log.Warn("skipping container that could not be inspected",
				"container", strings.TrimPrefix(firstName(item.Names), "/"), "id", short(item.ID), "error", err)
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Inspect returns a container with its image inspection attached.
func (r *Runtime) Inspect(ctx context.Context, id string) (*model.Container, error) {
	ci, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	ii, err := r.cli.ImageInspect(ctx, ci.Container.Image)
	if err != nil {
		// The image can legitimately be missing (removed manually). Recreation
		// still works from the stored config in most cases, so degrade
		// gracefully rather than dropping the container entirely.
		return &model.Container{Inspect: ci.Container, ImageInspect: image.InspectResponse{}}, nil
	}
	return &model.Container{Inspect: ci.Container, ImageInspect: ii.InspectResponse}, nil
}

// ImageID resolves a reference or ID to a local image ID.
func (r *Runtime) ImageID(ctx context.Context, ref string) (string, error) {
	ii, err := r.cli.ImageInspect(ctx, ref)
	if err != nil {
		if isNotFound(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return ii.ID, nil
}

// HasImage reports whether an image ID exists locally.
func (r *Runtime) HasImage(ctx context.Context, id string) bool {
	_, err := r.cli.ImageInspect(ctx, id)
	return err == nil
}

// Pull downloads imageRef and returns the resulting local image ID.
func (r *Runtime) Pull(ctx context.Context, ref, registryAuth string) (string, error) {
	resp, err := r.cli.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Close()

	// Wait consumes the progress stream and surfaces pull errors that would
	// otherwise be hidden inside the JSON progress messages.
	if err := resp.Wait(ctx); err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	id, err := r.ImageID(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect pulled image %s: %w", ref, err)
	}
	return id, nil
}

// HealthOptions controls post-start verification.
type HealthOptions struct {
	// Timeout bounds the total wait.
	Timeout time.Duration
	// Settle is how long a container without a healthcheck must stay running.
	Settle time.Duration
	// Port, when set, is probed over TCP on the container's IP.
	Port int
	// URL is an optional HTTP path probed on Port.
	URL string
}

// Stop gracefully stops a container using its configured stop signal.
func (r *Runtime) Stop(ctx context.Context, c *model.Container, timeout time.Duration) error {
	if !c.Running() {
		return nil
	}
	opts := client.ContainerStopOptions{Signal: c.StopSignal()}
	if timeout > 0 {
		secs := int(timeout / time.Second)
		opts.Timeout = &secs
	}
	if _, err := r.cli.ContainerStop(ctx, c.ID(), opts); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("stop %s: %w", c.Name(), err)
	}
	return r.waitStopped(ctx, c.ID(), timeout+10*time.Second)
}

func (r *Runtime) waitStopped(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		if info.Container.State == nil || !info.Container.State.Running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not stop within %s", short(id), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// RecreateOptions controls a single container update.
type RecreateOptions struct {
	// ImageRef is the image reference (tag) to recreate with.
	ImageRef string
	// StopTimeout is the graceful stop window.
	StopTimeout time.Duration
	// Rollback restores the previous container if the new one fails.
	Rollback bool
	// ImageID is the local image ID that was just pulled. If set, the new
	// container must reference exactly this image; this closes the window
	// where another push could move the tag between pull and create.
	ImageID string
	// Health is the post-start verification policy.
	Health HealthOptions
	// RemoveVolumes removes the replaced container's anonymous volumes once
	// the new container is healthy. Named volumes are never touched.
	RemoveVolumes bool
}

// RecreateResult reports what happened during an update.
type RecreateResult struct {
	OldContainerID string
	NewContainerID string
	BackupName     string
	Snapshot       model.Snapshot
	RolledBack     bool
}

// Recreate replaces a container with a new one built from the same config but
// a different image. It is designed to never leave the service down: if the
// new container cannot be created or fails health checks, the previous
// container is restored.
func (r *Runtime) Recreate(ctx context.Context, c *model.Container, opts RecreateOptions) (RecreateResult, error) {
	snap := c.Snapshot()
	res := RecreateResult{OldContainerID: c.ID(), Snapshot: snap}

	if err := r.Stop(ctx, c, opts.StopTimeout); err != nil {
		return res, fmt.Errorf("stop old container: %w", err)
	}

	backup := ""
	if !c.AutoRemove() {
		backup = backupName(c.Name())
		if _, err := r.cli.ContainerRename(ctx, c.ID(), client.ContainerRenameOptions{NewName: backup}); err != nil {
			_ = r.start(ctx, c.ID())
			return res, fmt.Errorf("move old container aside: %w", err)
		}
		res.BackupName = backup
	}

	newID, err := r.CreateFromSnapshot(ctx, snap.ForImage(opts.ImageRef), c.Name(), true)
	if err != nil {
		// The half-created container holds the original name, so it must be
		// removed before the backup can be renamed back.
		r.removeContainer(ctx, newID, true, false)
		res.RolledBack = true
		if rbErr := r.restore(ctx, snap, c.Name(), backup); rbErr != nil {
			return res, fmt.Errorf("create new container: %w; rollback failed: %v", err, rbErr)
		}
		return res, fmt.Errorf("create new container: %w (rolled back)", err)
	}
	res.NewContainerID = newID

	if opts.ImageID != "" {
		info, err := r.cli.ContainerInspect(ctx, newID, client.ContainerInspectOptions{})
		if err != nil {
			r.removeContainer(ctx, newID, true, false)
			res.RolledBack = true
			if rbErr := r.restore(ctx, snap, c.Name(), backup); rbErr != nil {
				return res, fmt.Errorf("inspect new container: %w; rollback failed: %v", err, rbErr)
			}
			return res, fmt.Errorf("inspect new container: %w (rolled back)", err)
		}
		if got := info.Container.Image; got != opts.ImageID {
			r.log.Warn("tag moved between pull and create",
				"container", c.Name(), "pulled", short(opts.ImageID), "created", short(got))
			r.removeContainer(ctx, newID, true, false)
			res.RolledBack = true
			if rbErr := r.restore(ctx, snap, c.Name(), backup); rbErr != nil {
				return res, fmt.Errorf("image changed during update: %w; rollback failed: %v", ErrImageChanged, rbErr)
			}
			return res, fmt.Errorf("image changed between pull and create (rolled back)")
		}
	}

	if err := r.WaitHealthy(ctx, newID, opts.Health); err != nil {
		if !opts.Rollback {
			return res, fmt.Errorf("verify new container: %w", err)
		}
		r.removeContainer(ctx, newID, true, false)
		res.RolledBack = true
		if rbErr := r.restore(ctx, snap, c.Name(), backup); rbErr != nil {
			return res, fmt.Errorf("verify new container: %w; rollback failed: %v", err, rbErr)
		}
		return res, fmt.Errorf("verify new container: %w (rolled back)", err)
	}

	// Success: the stopped backup is no longer needed. The image is retained
	// by the caller according to its keep_images policy.
	if backup != "" {
		r.removeContainer(ctx, c.ID(), false, opts.RemoveVolumes)
	}
	return res, nil
}

// restore brings the previous container back after a failed update.
func (r *Runtime) restore(ctx context.Context, snap model.Snapshot, name, backup string) error {
	if backup != "" {
		if _, err := r.cli.ContainerRename(ctx, backup, client.ContainerRenameOptions{NewName: name}); err != nil {
			return fmt.Errorf("rename backup back to %s: %w", name, err)
		}
		info, err := r.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if info.Container.State == nil || !info.Container.State.Running {
			if err := r.start(ctx, info.Container.ID); err != nil {
				return fmt.Errorf("restart previous container: %w", err)
			}
		}
		return nil
	}
	// The old container was created with --rm and the daemon removed it when
	// it stopped. Rebuild it from the snapshot and the still-present image.
	if snap.ImageID == "" {
		return errors.New("previous container was auto-removed and no snapshot image is available")
	}
	if _, err := r.CreateFromSnapshot(ctx, snap.ForImage(snap.ImageID), name, true); err != nil {
		return fmt.Errorf("recreate auto-removed container: %w", err)
	}
	return nil
}

// CreateFromSnapshot creates (and optionally starts) a container from a stored
// snapshot, naming it name.
func (r *Runtime) CreateFromSnapshot(ctx context.Context, snap model.Snapshot, name string, start bool) (string, error) {
	if snap.Config == nil || snap.HostConfig == nil {
		return "", errors.New("snapshot is missing container configuration")
	}
	cfg := snap.Config
	hc := snap.HostConfig
	if cfg.Image == "" {
		cfg.Image = snap.ImageRef
	}

	// Config.Image carries the reference; the client rejects setting both
	// Image and Config.Image.
	create := client.ContainerCreateOptions{
		Config:     cfg,
		HostConfig: hc,
		Name:       name,
	}

	var extraNetworks map[string]*network.EndpointSettings
	if !hc.NetworkMode.IsHost() && !hc.NetworkMode.IsNone() && !hc.NetworkMode.IsContainer() && snap.Networking != nil && len(snap.Networking.EndpointsConfig) > 0 {
		primary := primaryNetwork(hc.NetworkMode, snap.Networking)
		create.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				primary: snap.Networking.EndpointsConfig[primary],
			},
		}
		if len(snap.Networking.EndpointsConfig) > 1 {
			extraNetworks = make(map[string]*network.EndpointSettings, len(snap.Networking.EndpointsConfig)-1)
			for netName, ep := range snap.Networking.EndpointsConfig {
				if netName != primary {
					extraNetworks[netName] = ep
				}
			}
		}
	}

	created, err := r.cli.ContainerCreate(ctx, create)
	if err != nil {
		return "", err
	}
	for netName, ep := range extraNetworks {
		if _, err := r.cli.NetworkConnect(ctx, netName, client.NetworkConnectOptions{Container: created.ID, EndpointConfig: ep}); err != nil {
			r.removeContainer(ctx, created.ID, true, false)
			return "", fmt.Errorf("connect %s: %w", netName, err)
		}
	}
	if start {
		if err := r.start(ctx, created.ID); err != nil {
			return created.ID, err
		}
	}
	return created.ID, nil
}

// primaryNetwork picks the network to attach at create time. Docker's create
// API only accepts a single endpoint, hence the two-phase attach.
func primaryNetwork(mode container.NetworkMode, nc *network.NetworkingConfig) string {
	if name := mode.NetworkName(); name != "" && nc.EndpointsConfig[name] != nil {
		return name
	}
	for name := range nc.EndpointsConfig {
		return name
	}
	return ""
}

func (r *Runtime) start(ctx context.Context, id string) error {
	if _, err := r.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container %s: %w", short(id), err)
	}
	return nil
}

func (r *Runtime) removeContainer(ctx context.Context, id string, force, removeVolumes bool) {
	if id == "" {
		return
	}
	_, err := r.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: force, RemoveVolumes: removeVolumes})
	if err != nil && !isNotFound(err) {
		r.log.Warn("could not remove container", "id", short(id), "error", err)
	}
}

// RemoveContainer removes a container, optionally with its anonymous volumes.
func (r *Runtime) RemoveContainer(ctx context.Context, id string, removeVolumes bool) error {
	_, err := r.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: removeVolumes})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("remove container %s: %w", short(id), err)
	}
	return nil
}

// RemoveImage removes an image by ID.
func (r *Runtime) RemoveImage(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, err := r.cli.ImageRemove(ctx, id, client.ImageRemoveOptions{Force: false, PruneChildren: true})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove image %s: %w", short(id), err)
	}
	return nil
}

// WaitHealthy blocks until the container is healthy, or fails if it exits,
// crash-loops, reports unhealthy, or exceeds the timeout.
func (r *Runtime) WaitHealthy(ctx context.Context, id string, opts HealthOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	settle := opts.Settle
	if settle <= 0 {
		settle = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	started := time.Now()

	var runningSince time.Time
	var initialRestarts = -1
	var lastProbe error

	for {
		info, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect new container: %w", err)
		}
		c := &model.Container{Inspect: info.Container}
		if initialRestarts < 0 {
			initialRestarts = c.RestartCount()
		}

		if c.RestartCount() > initialRestarts {
			return fmt.Errorf("container restarted %d time(s) during startup", c.RestartCount()-initialRestarts)
		}

		switch c.Health() {
		case "healthy":
			return nil
		case "unhealthy":
			return errors.New("container reported unhealthy")
		}

		if c.Running() {
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			if opts.Port > 0 {
				if err := probe(ctx, info.Container, opts); err == nil {
					return nil
				} else {
					lastProbe = err
				}
			} else if time.Since(runningSince) >= settle {
				return nil
			}
		} else {
			runningSince = time.Time{}
			if c.Status() == "exited" && time.Since(started) > settle {
				code := 0
				if info.Container.State != nil {
					code = info.Container.State.ExitCode
				}
				return fmt.Errorf("container exited during startup with code %d", code)
			}
		}

		if time.Now().After(deadline) {
			if lastProbe != nil {
				return fmt.Errorf("timed out after %s waiting for health (last probe: %v)", timeout, lastProbe)
			}
			return fmt.Errorf("timed out after %s waiting for container to become healthy", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func probe(ctx context.Context, info container.InspectResponse, opts HealthOptions) error {
	ip := containerIP(info)
	if ip == "" {
		return errors.New("container has no IP address")
	}
	addr := net.JoinHostPort(ip, fmt.Sprint(opts.Port))
	d := net.Dialer{Timeout: 3 * time.Second}
	if opts.URL == "" {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}

	path := opts.URL
	if u, err := parseURL(path); err == nil && u != "" {
		path = u
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("health endpoint returned %s", resp.Status)
	}
	return nil
}

// parseURL extracts the path from an absolute health URL. The host is ignored
// so a container label can never turn drydock into an SSRF proxy.
func parseURL(raw string) (string, error) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid health url: %w", err)
	}
	if u.Path == "" {
		return "/", nil
	}
	return u.Path, nil
}

func containerIP(info container.InspectResponse) string {
	if info.NetworkSettings == nil {
		return ""
	}
	for _, ep := range info.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		if ep.IPAddress.IsValid() && !ep.IPAddress.IsUnspecified() {
			return ep.IPAddress.String()
		}
	}
	return ""
}

func backupName(name string) string {
	// Docker names must match [a-zA-Z0-9][a-zA-Z0-9_.-]*; an underscore keeps
	// the name valid without colliding with user names.
	return fmt.Sprintf("%s_drydock_backup_%d", truncateName(name, 40), time.Now().Unix())
}

func truncateName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	return name[:maxLen]
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return cerrdefs.IsNotFound(err) || errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "No such")
}

// Rollback swaps the running container for a container rebuilt from a stored
// snapshot. It uses the snapshot's image ID (not its tag), so the rollback
// target cannot be silently replaced by a newer push.
func (r *Runtime) Rollback(ctx context.Context, current *model.Container, target model.Snapshot, health HealthOptions) (string, error) {
	if target.ImageID == "" {
		return "", errors.New("rollback snapshot has no image ID")
	}
	if !r.HasImage(ctx, target.ImageID) {
		return "", fmt.Errorf("previous image %s is no longer available locally; cannot roll back safely", short(target.ImageID))
	}

	currentSnap := current.Snapshot()
	if err := r.Stop(ctx, current, 30*time.Second); err != nil {
		return "", fmt.Errorf("stop current container: %w", err)
	}

	backup := ""
	if !current.AutoRemove() {
		backup = backupName(current.Name())
		if _, err := r.cli.ContainerRename(ctx, current.ID(), client.ContainerRenameOptions{NewName: backup}); err != nil {
			_ = r.start(ctx, current.ID())
			return "", fmt.Errorf("move current container aside: %w", err)
		}
	}

	newID, err := r.CreateFromSnapshot(ctx, target.ForImage(target.ImageID), current.Name(), true)
	if err != nil {
		if rbErr := r.restore(ctx, currentSnap, current.Name(), backup); rbErr != nil {
			return "", fmt.Errorf("create rollback container: %w; restoring current state failed: %v", err, rbErr)
		}
		return "", fmt.Errorf("create rollback container: %w", err)
	}

	if err := r.WaitHealthy(ctx, newID, health); err != nil {
		r.removeContainer(ctx, newID, true, false)
		if rbErr := r.restore(ctx, currentSnap, current.Name(), backup); rbErr != nil {
			return "", fmt.Errorf("rollback container unhealthy: %w; restoring current state failed: %v", err, rbErr)
		}
		return "", fmt.Errorf("rollback container unhealthy: %w", err)
	}

	if backup != "" {
		r.removeContainer(ctx, current.ID(), false, false)
	}
	return newID, nil
}
