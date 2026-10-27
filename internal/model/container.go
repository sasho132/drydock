// Package model contains drydock's internal representation of a Docker
// container and the helpers needed to recreate one losslessly.
package model

import (
	"encoding/json"
	"net/netip"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
)

// Container couples a container's runtime inspection with the inspection of
// the image it was created from. Both are needed to safely recreate it.
type Container struct {
	Inspect      container.InspectResponse
	ImageInspect image.InspectResponse
}

// Name returns the container name without Docker's leading slash.
func (c *Container) Name() string { return strings.TrimPrefix(c.Inspect.Name, "/") }

// ID returns the full container ID.
func (c *Container) ID() string { return c.Inspect.ID }

// ShortID returns the first 12 characters of the container ID.
func (c *Container) ShortID() string { return short(c.Inspect.ID) }

// ImageID returns the content-addressable ID of the running image.
func (c *Container) ImageID() string { return c.Inspect.Image }

// LabelOriginImage is a reserved label that remembers the tag a container was
// originally created from when drydock had to create it from an image ID
// (rollback). It lets later updates keep tracking the tag.
const LabelOriginImage = "io.drydock.image-ref"

// ImageRef returns the image reference the container was created with
// (for example "nginx:1.27" or "ghcr.io/acme/app:sha-abc123").
func (c *Container) ImageRef() string {
	if c.Inspect.Config != nil && c.Inspect.Config.Image != "" {
		ref := c.Inspect.Config.Image
		if !IsImageIdentifier(ref) {
			return ref
		}
		if origin := c.Labels()[LabelOriginImage]; origin != "" {
			return origin
		}
		return ref
	}
	return c.Inspect.Image
}

// IsImageIdentifier reports whether s is an image ID or content digest rather
// than a tag reference.
func IsImageIdentifier(s string) bool {
	if strings.HasPrefix(s, "sha256:") || strings.HasPrefix(s, "sha512:") {
		return true
	}
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Labels returns the container's labels, never nil.
func (c *Container) Labels() map[string]string {
	if c.Inspect.Config == nil || c.Inspect.Config.Labels == nil {
		return map[string]string{}
	}
	return c.Inspect.Config.Labels
}

// Running reports whether the container is currently running.
func (c *Container) Running() bool { return c.Inspect.State != nil && c.Inspect.State.Running }

// Status returns Docker's container state string (running, exited, ...).
func (c *Container) Status() string {
	if c.Inspect.State == nil {
		return "unknown"
	}
	return string(c.Inspect.State.Status)
}

// Health returns the healthcheck status: healthy, unhealthy, starting, or
// "none" when the container defines no healthcheck.
func (c *Container) Health() string {
	if c.Inspect.State == nil || c.Inspect.State.Health == nil {
		return "none"
	}
	return string(c.Inspect.State.Health.Status)
}

// RestartCount returns how many times the container has been restarted.
func (c *Container) RestartCount() int { return c.Inspect.RestartCount }

// Created returns the container creation time, or the zero value if unknown.
func (c *Container) Created() time.Time {
	t, err := time.Parse(time.RFC3339Nano, c.Inspect.Created)
	if err != nil {
		return time.Time{}
	}
	return t
}

// AutoRemove reports whether the container is configured with --rm.
func (c *Container) AutoRemove() bool {
	return c.Inspect.HostConfig != nil && c.Inspect.HostConfig.AutoRemove
}

// StopSignal returns the configured stop signal, defaulting to SIGTERM.
func (c *Container) StopSignal() string {
	if c.Inspect.Config != nil && c.Inspect.Config.StopSignal != "" {
		return c.Inspect.Config.StopSignal
	}
	return "SIGTERM"
}

// StopTimeout returns the configured stop timeout in seconds, or 0 when unset.
func (c *Container) StopTimeout() int {
	if c.Inspect.Config != nil && c.Inspect.Config.StopTimeout != nil {
		return *c.Inspect.Config.StopTimeout
	}
	return 0
}

// Pinned reports whether the image reference is digest-pinned (repo@sha256:...),
// in which case the tag can never move to a new digest.
func (c *Container) Pinned() bool { return strings.Contains(c.ImageRef(), "@") }

// NormalizedName returns the fully qualified repository name
// (e.g. "docker.io/library/nginx") used to match registry digests.
func (c *Container) NormalizedName() string {
	name, _ := splitRef(c.ImageRef())
	return name
}

// Tag returns the tag portion of the image reference, defaulting to "latest".
func (c *Container) Tag() string {
	_, tag := splitRef(c.ImageRef())
	if tag == "" {
		return "latest"
	}
	return tag
}

// LocalDigests returns the digest portion of every RepoDigest that refers to
// this container's repository, without the "sha256:" prefix stripped.
func (c *Container) LocalDigests() []string {
	repo := c.NormalizedName()
	if repo == "" {
		return nil
	}
	var out []string
	for _, rd := range c.ImageInspect.RepoDigests {
		at := strings.LastIndex(rd, "@")
		if at < 0 {
			continue
		}
		name, normalizeErr := normalize(rd[:at])
		if normalizeErr != nil || name != repo {
			continue
		}
		out = append(out, rd[at+1:])
	}
	return out
}

// Platform returns the image's os/architecture/variant.
func (c *Container) Platform() (os, arch, variant string) {
	return c.ImageInspect.Os, c.ImageInspect.Architecture, c.ImageInspect.Variant
}

// CreatedFromImage returns the image configuration the container was built
// from. It may be nil for images that are no longer present locally.
func (c *Container) CreatedFromImage() *image.InspectResponse {
	if c.ImageInspect.ID == "" {
		return nil
	}
	return &c.ImageInspect
}

// Links returns the legacy container links, normalised to "source:alias".
func (c *Container) Links() []string {
	if c.Inspect.HostConfig == nil {
		return nil
	}
	out := make([]string, 0, len(c.Inspect.HostConfig.Links))
	for _, l := range c.Inspect.HostConfig.Links {
		out = append(out, NormalizeLink(l))
	}
	return out
}

// NormalizeLink converts an inspected link (either "/db:/web/db" or
// "db:alias") into the "db:alias" form accepted by the create API.
func NormalizeLink(link string) string {
	src, dst, ok := strings.Cut(link, ":")
	if !ok {
		return strings.TrimPrefix(link, "/")
	}
	src = strings.TrimPrefix(src, "/")
	dst = strings.TrimPrefix(dst, "/")
	if i := strings.LastIndex(dst, "/"); i >= 0 {
		dst = dst[i+1:]
	}
	if dst == "" {
		return src
	}
	return src + ":" + dst
}

// CreateConfig builds a fresh container config for recreation. Fields that
// simply mirror the old image's defaults are removed so the new image's
// defaults take effect, while genuine user overrides are preserved.
// The receiver's inspect data is never mutated.
func (c *Container) CreateConfig(newImageRef string) *container.Config {
	var cfg container.Config
	if c.Inspect.Config != nil {
		cfg = *clone(c.Inspect.Config)
	}
	img := c.imageConfig()

	if img != nil {
		if cfg.WorkingDir == img.WorkingDir {
			cfg.WorkingDir = ""
		}
		if cfg.User == img.User {
			cfg.User = ""
		}
		if equalStrings(cfg.Entrypoint, img.Entrypoint) {
			cfg.Entrypoint = nil
			if equalStrings(cfg.Cmd, img.Cmd) {
				cfg.Cmd = nil
			}
		}
		cfg.Env = subtractStrings(cfg.Env, img.Env)
		cfg.Labels = subtractMap(cfg.Labels, img.Labels)
		cfg.Volumes = subtractSet(cfg.Volumes, img.Volumes)
		cfg.ExposedPorts = subtractPortSet(cfg.ExposedPorts, img.ExposedPorts)
		cfg.Healthcheck = mergeHealthcheck(cfg.Healthcheck, img.Healthcheck)
	}

	if cfg.ExposedPorts == nil {
		cfg.ExposedPorts = network.PortSet{}
	}
	if c.Inspect.HostConfig != nil {
		for port := range c.Inspect.HostConfig.PortBindings {
			cfg.ExposedPorts[port] = struct{}{}
		}
		if nm := c.Inspect.HostConfig.NetworkMode; nm.IsContainer() || nm.IsHost() {
			cfg.Hostname = ""
		}
	}

	cfg.Image = newImageRef
	return &cfg
}

// CreateHostConfig returns a defensive copy of the host config with links
// rewritten into the form accepted by the create API.
func (c *Container) CreateHostConfig() *container.HostConfig {
	if c.Inspect.HostConfig == nil {
		return &container.HostConfig{}
	}
	hc := clone(c.Inspect.HostConfig)
	for i, l := range hc.Links {
		hc.Links[i] = NormalizeLink(l)
	}
	return hc
}

// CreateNetworkingConfig returns a sanitised copy of the container's network
// attachment. Runtime-only fields (IP, gateway, endpoint ID) are cleared so the
// daemon can allocate them afresh; stale container-ID aliases are dropped.
func (c *Container) CreateNetworkingConfig() *network.NetworkingConfig {
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	if c.Inspect.NetworkSettings == nil {
		return nc
	}
	shortID := c.ShortID()
	for name, ep := range c.Inspect.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		clean := ep.Copy()
		clean.EndpointID = ""
		clean.NetworkID = ""
		clean.Gateway = netip.Addr{}
		clean.IPAddress = netip.Addr{}
		clean.IPPrefixLen = 0
		clean.IPv6Gateway = netip.Addr{}
		clean.GlobalIPv6Address = netip.Addr{}
		clean.GlobalIPv6PrefixLen = 0
		clean.MacAddress = nil
		aliases := clean.Aliases[:0]
		for _, a := range clean.Aliases {
			if a == shortID || a == c.ID() {
				continue
			}
			aliases = append(aliases, a)
		}
		clean.Aliases = aliases
		nc.EndpointsConfig[name] = clean
	}
	return nc
}

func (c *Container) imageConfig() *imageDefaults {
	ic := c.ImageInspect.Config
	if ic == nil {
		return nil
	}
	return &imageDefaults{
		User:         ic.User,
		WorkingDir:   ic.WorkingDir,
		Env:          ic.Env,
		Entrypoint:   ic.Entrypoint,
		Cmd:          ic.Cmd,
		Labels:       ic.Labels,
		Volumes:      ic.Volumes,
		ExposedPorts: ic.ExposedPorts,
		Healthcheck:  ic.Healthcheck,
	}
}

// imageDefaults is the subset of an image's config that matters when deciding
// whether a container config field is a user override or an inherited default.
type imageDefaults struct {
	User         string
	WorkingDir   string
	Env          []string
	Entrypoint   []string
	Cmd          []string
	Labels       map[string]string
	Volumes      map[string]struct{}
	ExposedPorts map[string]struct{}
	Healthcheck  *container.HealthConfig
}

func splitRef(ref string) (name, tag string) {
	if named, err := reference.ParseNormalizedNamed(ref); err == nil {
		name = named.Name()
		if tagged, ok := named.(reference.Tagged); ok {
			tag = tagged.Tag()
		}
		return name, tag
	}
	// Fall back to a best-effort split for malformed references.
	ref = strings.TrimPrefix(ref, "docker.io/")
	if at := strings.Index(ref, "@"); at >= 0 {
		return ref[:at], ""
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

func normalize(name string) (string, error) {
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return "", err
	}
	return named.Name(), nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func subtractStrings(current, remove []string) []string {
	if len(remove) == 0 {
		return current
	}
	drop := make(map[string]struct{}, len(remove))
	for _, s := range remove {
		drop[s] = struct{}{}
	}
	out := make([]string, 0, len(current))
	for _, s := range current {
		if _, ok := drop[s]; ok {
			continue
		}
		out = append(out, s)
	}
	return out
}

func subtractMap(current, remove map[string]string) map[string]string {
	if len(current) == 0 {
		return current
	}
	out := make(map[string]string, len(current))
	for k, v := range current {
		if rv, ok := remove[k]; ok && rv == v {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func subtractSet(current, remove map[string]struct{}) map[string]struct{} {
	if len(current) == 0 {
		return current
	}
	out := make(map[string]struct{}, len(current))
	for k := range current {
		if _, ok := remove[k]; ok {
			continue
		}
		out[k] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func subtractPortSet(current network.PortSet, remove map[string]struct{}) network.PortSet {
	if len(current) == 0 {
		return current
	}
	out := make(network.PortSet, len(current))
	for p := range current {
		if _, ok := remove[p.String()]; ok {
			continue
		}
		out[p] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeHealthcheck strips values that merely repeat the old image's
// healthcheck, so a new image can change its own defaults. Returns nil when
// nothing container-specific remains.
func mergeHealthcheck(cur *container.HealthConfig, img *container.HealthConfig) *container.HealthConfig {
	if cur == nil {
		return nil
	}
	out := clone(cur)
	if img != nil {
		if equalStrings(out.Test, img.Test) {
			out.Test = nil
		}
		if out.Retries == img.Retries {
			out.Retries = 0
		}
		if out.Interval == img.Interval {
			out.Interval = 0
		}
		if out.Timeout == img.Timeout {
			out.Timeout = 0
		}
		if out.StartPeriod == img.StartPeriod {
			out.StartPeriod = 0
		}
		if out.StartInterval == img.StartInterval {
			out.StartInterval = 0
		}
	}
	if len(out.Test) == 0 && out.Retries == 0 && out.Interval == 0 &&
		out.Timeout == 0 && out.StartPeriod == 0 && out.StartInterval == 0 {
		return nil
	}
	return out
}

func clone[T any](in *T) *T {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		return in
	}
	return &out
}

// Snapshot is a serialisable record of everything needed to recreate a
// container. Configs are stored in already-sanitised create form so a rollback
// can be performed without re-inspecting the original container.
type Snapshot struct {
	ContainerID string                    `json:"container_id"`
	Name        string                    `json:"name"`
	ImageRef    string                    `json:"image_ref"`
	ImageID     string                    `json:"image_id"`
	Config      *container.Config         `json:"config,omitempty"`
	HostConfig  *container.HostConfig     `json:"host_config,omitempty"`
	Networking  *network.NetworkingConfig `json:"networking,omitempty"`
	CreatedAt   time.Time                 `json:"created_at"`
}

// Snapshot captures the container in its current state, ready for recreation.
func (c *Container) Snapshot() Snapshot {
	return Snapshot{
		ContainerID: c.ID(),
		Name:        c.Name(),
		ImageRef:    c.ImageRef(),
		ImageID:     c.ImageID(),
		Config:      c.CreateConfig(c.ImageRef()),
		HostConfig:  c.CreateHostConfig(),
		Networking:  c.CreateNetworkingConfig(),
		CreatedAt:   c.Created(),
	}
}

// ForImage returns a copy of the snapshot whose config targets imageRef.
//
// When imageRef is an image ID (used for rollback, so the exact previous image
// is restored), the original tag is preserved in a reserved label. Without it
// the recreated container would permanently lose track of its tag and could
// never be updated again.
func (s Snapshot) ForImage(imageRef string) Snapshot {
	out := s
	if s.Config != nil {
		out.Config = clone(s.Config)
		out.Config.Image = imageRef
		if IsImageIdentifier(imageRef) && s.ImageRef != "" {
			if out.Config.Labels == nil {
				out.Config.Labels = map[string]string{}
			}
			out.Config.Labels[LabelOriginImage] = s.ImageRef
		} else if out.Config.Labels != nil {
			delete(out.Config.Labels, LabelOriginImage)
		}
	}
	if s.HostConfig != nil {
		out.HostConfig = clone(s.HostConfig)
	}
	if s.Networking != nil {
		out.Networking = clone(s.Networking)
	}
	return out
}
