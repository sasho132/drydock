package model

import (
	"testing"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
)

func imageInspect(cfg *dockerspec.DockerOCIImageConfig) image.InspectResponse {
	return image.InspectResponse{ID: "sha256:image", Config: cfg}
}

func TestCreateConfigDropsImageDefaultsKeepsOverrides(t *testing.T) {
	imgCfg := &dockerspec.DockerOCIImageConfig{}
	imgCfg.WorkingDir = "/app"
	imgCfg.User = "app"
	imgCfg.Env = []string{"PATH=/usr/bin"}
	imgCfg.Cmd = []string{"serve"}
	imgCfg.Entrypoint = []string{"/entrypoint"}
	imgCfg.Labels = map[string]string{"vendor": "acme"}

	c := &Container{
		Inspect: container.InspectResponse{
			ID:   "abc",
			Name: "/web",
			Config: &container.Config{
				Image:      "acme/web:1.0",
				WorkingDir: "/app",
				User:       "app",
				Env:        []string{"PATH=/usr/bin", "DEBUG=true"},
				Cmd:        []string{"serve"},
				Entrypoint: []string{"/entrypoint"},
				Labels:     map[string]string{"vendor": "acme", "tier": "frontend"},
			},
			HostConfig: &container.HostConfig{},
			State:      &container.State{Running: true},
		},
		ImageInspect: imageInspect(imgCfg),
	}

	got := c.CreateConfig("acme/web:2.0")

	if got.Image != "acme/web:2.0" {
		t.Fatalf("image = %q, want acme/web:2.0", got.Image)
	}
	if got.WorkingDir != "" {
		t.Errorf("WorkingDir should fall back to the new image, got %q", got.WorkingDir)
	}
	if got.User != "" {
		t.Errorf("User should fall back to the new image, got %q", got.User)
	}
	if len(got.Cmd) != 0 {
		t.Errorf("Cmd should fall back to the new image, got %v", got.Cmd)
	}
	if len(got.Entrypoint) != 0 {
		t.Errorf("Entrypoint should fall back to the new image, got %v", got.Entrypoint)
	}
	if len(got.Env) != 1 || got.Env[0] != "DEBUG=true" {
		t.Errorf("Env = %v, want only the user override", got.Env)
	}
	if len(got.Labels) != 1 || got.Labels["tier"] != "frontend" {
		t.Errorf("Labels = %v, want only the user override", got.Labels)
	}

	// The original inspect data must never be mutated.
	if c.Inspect.Config.WorkingDir != "/app" || len(c.Inspect.Config.Env) != 2 {
		t.Errorf("CreateConfig mutated the source container: %+v", c.Inspect.Config)
	}
}

func TestCreateConfigPreservesUserOverrides(t *testing.T) {
	imgCfg := &dockerspec.DockerOCIImageConfig{}
	imgCfg.WorkingDir = "/app"
	imgCfg.Cmd = []string{"serve"}

	c := &Container{
		Inspect: container.InspectResponse{
			Name: "/web",
			Config: &container.Config{
				Image:      "acme/web:1.0",
				WorkingDir: "/custom",
				Cmd:        []string{"serve", "--debug"},
			},
			HostConfig: &container.HostConfig{},
		},
		ImageInspect: imageInspect(imgCfg),
	}

	got := c.CreateConfig("acme/web:2.0")
	if got.WorkingDir != "/custom" {
		t.Errorf("WorkingDir override lost: %q", got.WorkingDir)
	}
	if len(got.Cmd) != 2 || got.Cmd[1] != "--debug" {
		t.Errorf("Cmd override lost: %v", got.Cmd)
	}
}

func TestCreateConfigAddsHostPortBindings(t *testing.T) {
	c := &Container{
		Inspect: container.InspectResponse{
			Config: &container.Config{Image: "app:1"},
			HostConfig: &container.HostConfig{
				PortBindings: network.PortMap{
					network.MustParsePort("8080/tcp"): []network.PortBinding{{HostPort: "80"}},
				},
			},
		},
	}
	got := c.CreateConfig("app:2")
	if _, ok := got.ExposedPorts[network.MustParsePort("8080/tcp")]; !ok {
		t.Errorf("port binding was not converted into an exposed port: %v", got.ExposedPorts)
	}
}

func TestNormalizeLink(t *testing.T) {
	cases := map[string]string{
		"/db:/web/db": "db:db",
		"db:alias":    "db:alias",
		"/db":         "db",
	}
	for in, want := range cases {
		if got := NormalizeLink(in); got != want {
			t.Errorf("NormalizeLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalDigestsMatchesRepository(t *testing.T) {
	c := &Container{
		Inspect: container.InspectResponse{
			Config: &container.Config{Image: "nginx:1.27"},
		},
		ImageInspect: image.InspectResponse{
			RepoDigests: []string{
				"nginx@sha256:aaa",
				"docker.io/library/nginx@sha256:bbb",
				"alpine@sha256:ccc",
			},
		},
	}
	got := c.LocalDigests()
	if len(got) != 2 || got[0] != "sha256:aaa" || got[1] != "sha256:bbb" {
		t.Fatalf("LocalDigests() = %v", got)
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	c := &Container{
		Inspect: container.InspectResponse{
			ID:     "abc",
			Name:   "/web",
			Image:  "sha256:old",
			Config: &container.Config{Image: "app:1", Env: []string{"A=1"}},
			HostConfig: &container.HostConfig{
				Links: []string{"/db:/web/db"},
			},
		},
		ImageInspect: image.InspectResponse{ID: "sha256:old"},
	}
	snap := c.Snapshot()
	if snap.ImageID != "sha256:old" || snap.ImageRef != "app:1" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if len(snap.HostConfig.Links) != 1 || snap.HostConfig.Links[0] != "db:db" {
		t.Errorf("links not normalised in snapshot: %v", snap.HostConfig.Links)
	}

	modified := snap.ForImage("sha256:old")
	modified.Config.Env[0] = "A=2"
	if c.Inspect.Config.Env[0] != "A=1" {
		t.Errorf("ForImage aliased the original config")
	}
}

func TestSnapshotForImageIDPreservesOriginTag(t *testing.T) {
	c := &Container{
		Inspect: container.InspectResponse{
			ID:     "abc",
			Name:   "/web",
			Image:  "sha256:current",
			Config: &container.Config{Image: "acme/web:latest"},
		},
		ImageInspect: image.InspectResponse{ID: "sha256:current"},
	}
	snap := c.Snapshot()

	restored := snap.ForImage("sha256:previous")
	if restored.Config.Image != "sha256:previous" {
		t.Fatalf("config image = %q", restored.Config.Image)
	}
	if got := restored.Config.Labels[LabelOriginImage]; got != "acme/web:latest" {
		t.Fatalf("origin label = %q", got)
	}
	recovered := &Container{Inspect: container.InspectResponse{Config: restored.Config}}
	if got := recovered.ImageRef(); got != "acme/web:latest" {
		t.Fatalf("ImageRef after rollback = %q, want the tag", got)
	}

	// A subsequent tag-based recreate clears the reserved label again.
	next := snap.ForImage("acme/web:latest")
	if _, ok := next.Config.Labels[LabelOriginImage]; ok {
		t.Error("reserved label should be removed when recreating from a tag")
	}
}
