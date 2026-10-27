package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sasho/drydock/internal/config"
)

func newTestClient(auths map[string]config.Auth) *Client {
	return New(config.RegistryConfig{
		Auths:     auths,
		Retries:   2,
		Timeout:   config.Duration{Duration: 5 * time.Second},
		PlainHTTP: true,
	}, nil)
}

func TestResolvePicksPlatformDigest(t *testing.T) {
	var sawAuth atomic.Bool
	var srvURL string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:app:pull" {
			t.Errorf("unexpected scope %q", r.URL.Query().Get("scope"))
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	})
	mux.HandleFunc("/v2/app/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			sawAuth.Store(true)
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test"`, srvURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:indexdigest")
		json.NewEncoder(w).Encode(map[string]any{
			"mediaType": "application/vnd.oci.image.index.v1+json",
			"manifests": []map[string]any{
				{"digest": "sha256:amd64", "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
				{"digest": "sha256:arm64v8", "platform": map[string]string{"os": "linux", "architecture": "arm64", "variant": "v8"}},
			},
		})
	})

	ref := strings.TrimPrefix(srv.URL, "http://") + "/app:latest"
	res, err := newTestClient(nil).Resolve(context.Background(), ref, Platform{OS: "linux", Arch: "arm64", Variant: "v8"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth.Load() {
		t.Error("registry was never challenged for credentials")
	}
	if res.Digest != "sha256:indexdigest" {
		t.Errorf("index digest = %q", res.Digest)
	}
	if res.PlatformDigest != "sha256:arm64v8" {
		t.Errorf("platform digest = %q, want sha256:arm64v8", res.PlatformDigest)
	}
	if res.Repository != "app" {
		t.Errorf("repository = %q", res.Repository)
	}
}

func TestResolveSingleManifest(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/v2/team/api/manifests/1.2.3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:single")
		json.NewEncoder(w).Encode(map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json"})
	})

	ref := strings.TrimPrefix(srv.URL, "http://") + "/team/api:1.2.3"
	res, err := newTestClient(nil).Resolve(context.Background(), ref, Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != "sha256:single" || res.PlatformDigest != "sha256:single" {
		t.Errorf("unexpected result %+v", res)
	}
}

func TestResolvePinnedDoesNotHitNetwork(t *testing.T) {
	c := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("network must not be used")
	})}}
	res, err := c.Resolve(context.Background(), "nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Platform{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pinned || res.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestResolveRetriesOnRateLimit(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var calls atomic.Int32
	mux.HandleFunc("/v2/app/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:afterretry")
		json.NewEncoder(w).Encode(map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json"})
	})

	ref := strings.TrimPrefix(srv.URL, "http://") + "/app:latest"
	res, err := newTestClient(nil).Resolve(context.Background(), ref, Platform{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != "sha256:afterretry" {
		t.Errorf("digest = %q", res.Digest)
	}
	if calls.Load() < 2 {
		t.Errorf("expected a retry, got %d calls", calls.Load())
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:a/b:pull"`)
	if scheme != "Bearer" {
		t.Fatalf("scheme = %q", scheme)
	}
	if params["realm"] != "https://auth.example/token" {
		t.Errorf("realm = %q", params["realm"])
	}
	if params["scope"] != "repository:a/b:pull" {
		t.Errorf("scope = %q", params["scope"])
	}
}

func TestDockerAuthDockerHub(t *testing.T) {
	c := newTestClient(map[string]config.Auth{
		"docker.io": {Username: "bot", Password: "token"},
	})
	raw := decodeAuth(t, c.DockerAuth("sasho1320/my-app:latest"))
	if raw["username"] != "bot" || raw["password"] != "token" {
		t.Errorf("unexpected auth payload %v", raw)
	}
	if raw["serveraddress"] != "https://index.docker.io/v1/" {
		t.Errorf("serveraddress = %q", raw["serveraddress"])
	}
}

func TestDockerAuthUsesConfiguredCredentials(t *testing.T) {
	c := newTestClient(map[string]config.Auth{
		"ghcr.io": {Username: "bot", Password: "secret"},
	})
	got := c.DockerAuth("ghcr.io/acme/app:latest")
	if got == "" {
		t.Fatal("expected credentials")
	}
	raw := decodeAuth(t, got)
	if raw["username"] != "bot" || raw["password"] != "secret" || raw["serveraddress"] != "ghcr.io" {
		t.Errorf("unexpected auth payload %v", raw)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func decodeAuth(t *testing.T, s string) map[string]string {
	t.Helper()
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal auth: %v", err)
	}
	return m
}

func TestResolveUnauthorizedWithoutCredentials(t *testing.T) {
	var srvURL string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token": "anonymous"})
	})
	mux.HandleFunc("/v2/app/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test"`, srvURL))
		w.WriteHeader(http.StatusUnauthorized)
	})

	ref := strings.TrimPrefix(srv.URL, "http://") + "/app:latest"
	_, err := newTestClient(nil).Resolve(context.Background(), ref, Platform{})
	if err == nil {
		t.Fatal("expected an authorization error")
	}
	if !strings.Contains(err.Error(), "may not exist or may be private") {
		t.Fatalf("error is misleading: %v", err)
	}
	if strings.Contains(err.Error(), "giving up") {
		t.Fatalf("401 should not be retried until exhaustion: %v", err)
	}
}

func TestDockerHubCredentialAliases(t *testing.T) {
	// Users write any of these keys for Docker Hub. The digest check contacts
	// registry-1.docker.io, so every alias must resolve.
	aliases := []string{
		"docker.io",
		"registry-1.docker.io",
		"index.docker.io",
		"https://index.docker.io/v1/",
	}
	for _, alias := range aliases {
		c := newTestClient(map[string]config.Auth{alias: {Username: "bot", Password: "token"}})
		creds := c.credentials("registry-1.docker.io")
		if creds.Username != "bot" {
			t.Errorf("credentials configured under %q were not found for registry-1.docker.io", alias)
		}
		if !c.hasCredentials("registry-1.docker.io") {
			t.Errorf("hasCredentials returned false for alias %q", alias)
		}
	}
}

func TestResolveSendsCredentialsToTokenEndpoint(t *testing.T) {
	var srvURL string
	var sawBasic bool
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "bot" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawBasic = true
		json.NewEncoder(w).Encode(map[string]string{"token": "private-token"})
	})
	mux.HandleFunc("/v2/app/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test"`, srvURL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:private")
		json.NewEncoder(w).Encode(map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json"})
	})

	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/app:latest"
	c := newTestClient(map[string]config.Auth{host: {Username: "bot", Password: "secret"}})
	res, err := c.Resolve(context.Background(), ref, Platform{})
	if err != nil {
		t.Fatal(err)
	}
	if !sawBasic {
		t.Error("token endpoint never received basic auth")
	}
	if res.Digest != "sha256:private" {
		t.Errorf("digest = %q", res.Digest)
	}
}
