// Package registry resolves image references to the digest currently published
// by a remote registry.
//
// Unlike a naive HEAD request, it understands multi-architecture images: for a
// manifest index it selects the platform-specific manifest that matches the
// container host, so the digest it reports is directly comparable with the
// digest of the image actually running.
package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/sasho/drydock/internal/config"
	"github.com/sasho/drydock/internal/version"
)

// mediaTypes are the manifest formats drydock understands, newest first.
var mediaTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// indexMediaTypes mark a manifest that lists platform-specific manifests.
var indexMediaTypes = map[string]bool{
	"application/vnd.oci.image.index.v1+json":                   true,
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
}

const (
	dockerHubDomain = "docker.io"
	dockerHubAPI    = "registry-1.docker.io"
	dockerHubAuth   = "https://index.docker.io/v1/"
)

// Platform identifies the target os/architecture/variant of a container.
type Platform struct {
	OS      string
	Arch    string
	Variant string
}

// Result describes the remote state of an image reference.
type Result struct {
	// Ref is the original reference.
	Ref string
	// Registry is the host that served the manifest.
	Registry string
	// Repository is the full repository path within the registry.
	Repository string
	// Tag is the tag used, or "" for digest-pinned references.
	Tag string
	// Digest is the digest of the top-level manifest (the index for
	// multi-arch images). This is what RepoDigests usually contains.
	Digest string
	// PlatformDigest is the digest of the manifest matching the requested
	// platform. It equals Digest for single-arch images.
	PlatformDigest string
	// Pinned reports whether the reference was digest-pinned.
	Pinned bool
}

// Credentials are the registry credentials drydock knows about.
type Credentials struct {
	Username      string
	Password      string
	IdentityToken string
}

// Client resolves remote manifests. It is safe for concurrent use.
type Client struct {
	http      *http.Client
	auths     map[string]config.Auth
	retries   int
	plain     bool
	log       *slog.Logger
	mu        sync.Mutex
	tokens    map[string]string
	dockerCfg map[string]Credentials
}

// New builds a registry client from configuration. It reads Docker's
// config.json once so credentials users already have keep working.
func New(cfg config.RegistryConfig, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	timeout := cfg.Timeout.Duration
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	// TLS verification is on unless the operator explicitly opts out for a
	// self-signed private registry on a trusted network.
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}

	return &Client{
		http:      &http.Client{Transport: transport, Timeout: timeout},
		auths:     cfg.Auths,
		retries:   max(cfg.Retries, 0),
		plain:     cfg.PlainHTTP,
		log:       log,
		tokens:    map[string]string{},
		dockerCfg: loadDockerConfig(),
	}
}

// Resolve fetches the current digest for imageRef. When the reference is
// already digest-pinned it is returned as-is without contacting the network.
func (c *Client) Resolve(ctx context.Context, imageRef string, p Platform) (Result, error) {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return Result{}, fmt.Errorf("parse image reference %q: %w", imageRef, err)
	}

	host := registryHost(named)
	// named.Name() is fully qualified, e.g. "docker.io/library/nginx" or
	// "localhost:5000/team/app"; the registry API wants only the path part.
	repo := strings.TrimPrefix(named.Name(), reference.Domain(named)+"/")

	res := Result{Ref: imageRef, Registry: host, Repository: repo, Tag: tagOf(named)}

	if canonical, ok := named.(reference.Canonical); ok {
		res.Pinned = true
		res.Digest = canonical.Digest().String()
		res.PlatformDigest = res.Digest
		return res, nil
	}

	scheme := "https"
	if c.plain {
		scheme = "http"
	}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, host, repo, res.Tag)

	body, mediaType, digest, err := c.getManifest(ctx, host, manifestURL, repo)
	if err != nil {
		return Result{}, err
	}
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	res.Digest = digest
	res.PlatformDigest = digest

	mt := mediaType
	if !indexMediaTypes[mt] && !strings.Contains(string(body), `"manifests"`) {
		return res, nil
	}

	var idx manifestIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return res, fmt.Errorf("decode manifest index for %q: %w", imageRef, err)
	}
	if match := pickPlatform(idx.Manifests, p); match != "" {
		res.PlatformDigest = match
	}
	return res, nil
}

func (c *Client) getManifest(ctx context.Context, host, manifestURL, repo string) (body []byte, mediaType, digest string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	accept := strings.Join(mediaTypes, ", ")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", version.UserAgent())

	res, err := c.doWithAuth(ctx, req, host, repo)
	if err != nil {
		return nil, "", "", err
	}
	defer res.Body.Close()

	body, err = io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, "", "", fmt.Errorf("read manifest for %s: %w", repo, err)
	}
	return body, res.Header.Get("Content-Type"), res.Header.Get("Docker-Content-Digest"), nil
}

// doWithAuth performs req, answering a 401 challenge and retrying transient
// failures with exponential backoff.
func (c *Client) doWithAuth(ctx context.Context, req *http.Request, host, repo string) (*http.Response, error) {
	var lastErr error
	unauthorized := 0
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt, lastErr); err != nil {
				return nil, err
			}
		}

		attemptReq := req.Clone(ctx)
		attemptReq.Header = req.Header.Clone()

		resp, err := c.sendWithAuth(ctx, attemptReq, host, repo)
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized:
			resp.Body.Close()
			c.dropToken(host, repo)
			hasCreds := c.hasCredentials(host)
			// A cached token may simply have expired. Retry once with a fresh
			// one; after that, a 401 is a real authorization failure.
			if hasCreds && unauthorized == 0 {
				unauthorized++
				lastErr = fmt.Errorf("registry %s rejected credentials", displayHost(host))
				continue
			}
			if !hasCreds {
				return nil, fmt.Errorf("registry %s denied access to %s (no credentials found; the repository may not exist or may be private)", displayHost(host), repo)
			}
			return nil, fmt.Errorf("registry %s rejected credentials for %s", displayHost(host), repo)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			resp.Body.Close()
			lastErr = &retryableError{status: resp.StatusCode, after: retryAfter}
		default:
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("registry %s returned %s for %s: %s",
				host, resp.Status, repo, strings.TrimSpace(string(snippet)))
		}
	}
	return nil, fmt.Errorf("registry %s: giving up after %d attempts: %w", host, c.retries+1, lastErr)
}

// sendWithAuth performs one logical request. If the registry answers 401 it
// obtains a token (or applies basic auth) and retries once. Every response is
// returned to the caller, which owns status handling and retries.
func (c *Client) sendWithAuth(ctx context.Context, req *http.Request, host, repo string) (*http.Response, error) {
	if tok := c.cachedToken(host, repo); tok != "" {
		req.Header.Set("Authorization", tok)
		return c.http.Do(req)
	}

	probe := req.Clone(ctx)
	probe.Header = req.Header.Clone()
	probe.Header.Del("Authorization")
	resp, err := c.http.Do(probe)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		// The probe is identical to the caller's request, so this response is
		// the real result (success or an error the caller should classify).
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()

	scheme, params := parseChallenge(challenge)
	if scheme == "" {
		return nil, fmt.Errorf("registry %s requires authentication but sent no challenge", host)
	}

	creds := c.credentials(host)
	switch strings.ToLower(scheme) {
	case "basic":
		if creds.Username == "" && creds.Password == "" {
			return nil, fmt.Errorf("registry %s requires basic auth but no credentials are configured", host)
		}
		req.SetBasicAuth(creds.Username, creds.Password)
	case "bearer":
		token, err := c.fetchToken(ctx, params, repo, creds)
		if err != nil {
			return nil, fmt.Errorf("obtain registry token: %w", err)
		}
		auth := "Bearer " + token
		c.storeToken(host, repo, auth)
		req.Header.Set("Authorization", auth)
	default:
		return nil, fmt.Errorf("registry %s uses unsupported auth scheme %q", host, scheme)
	}
	return c.http.Do(req)
}

func (c *Client) fetchToken(ctx context.Context, params map[string]string, repo string, creds Credentials) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("bearer challenge has no realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", version.UserAgent())
	if creds.Username != "" || creds.Password != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", errors.New("token endpoint returned an empty token")
}

// DockerAuth returns the base64-encoded credentials blob expected by the
// Docker Engine's image pull API for the registry hosting imageRef.
func (c *Client) DockerAuth(imageRef string) string {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return ""
	}
	host := reference.Domain(named)
	creds := c.credentials(host)
	if creds.Username == "" && creds.Password == "" && creds.IdentityToken == "" {
		return ""
	}
	serverAddress := host
	if host == dockerHubDomain {
		serverAddress = dockerHubAuth
	}
	ac := map[string]string{
		"username":      creds.Username,
		"password":      creds.Password,
		"serveraddress": serverAddress,
	}
	if creds.IdentityToken != "" {
		ac["identitytoken"] = creds.IdentityToken
	}
	b, _ := json.Marshal(ac)
	return base64.URLEncoding.EncodeToString(b)
}

func (c *Client) credentials(host string) Credentials {
	keys := []string{host}
	if host == dockerHubDomain || host == dockerHubAPI {
		// Docker Hub is addressed as registry-1.docker.io but users
		// (and Docker's own config.json) use several aliases.
		keys = append(keys, dockerHubDomain, "index.docker.io", dockerHubAuth)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		if a, ok := c.auths[k]; ok {
			return Credentials{Username: a.Username, Password: a.Password, IdentityToken: a.Token}
		}
	}
	for _, k := range keys {
		if a, ok := c.dockerCfg[k]; ok {
			return a
		}
	}
	return Credentials{}
}

// displayHost shortens Docker Hub's API host to the name users recognise.
func displayHost(host string) string {
	if host == dockerHubAPI {
		return dockerHubDomain
	}
	return host
}

func (c *Client) hasCredentials(host string) bool {
	creds := c.credentials(host)
	return creds.Username != "" || creds.Password != "" || creds.IdentityToken != ""
}

func (c *Client) cachedToken(host, repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[host+"|"+repo]
}

func (c *Client) storeToken(host, repo, tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[host+"|"+repo] = tok
}

func (c *Client) dropToken(host, repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, host+"|"+repo)
}

func registryHost(named reference.Named) string {
	domain := reference.Domain(named)
	if domain == dockerHubDomain {
		return dockerHubAPI
	}
	return domain
}

func tagOf(named reference.Named) string {
	if tagged, ok := named.(reference.Tagged); ok {
		return tagged.Tag()
	}
	return "latest"
}

type manifestIndex struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
}

type descriptor struct {
	MediaType string        `json:"mediaType"`
	Digest    string        `json:"digest"`
	Platform  *platformSpec `json:"platform"`
}

type platformSpec struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant"`
}

func pickPlatform(manifests []descriptor, p Platform) string {
	if len(manifests) == 0 {
		return ""
	}
	if p.OS == "" && p.Arch == "" {
		return ""
	}
	var fallback string
	for _, m := range manifests {
		if m.Platform == nil {
			continue
		}
		if p.OS != "" && m.Platform.OS != p.OS {
			continue
		}
		if p.Arch != "" && m.Platform.Architecture != p.Arch {
			continue
		}
		if p.Variant != "" && m.Platform.Variant != "" && m.Platform.Variant != p.Variant {
			continue
		}
		if p.Variant != "" && m.Platform.Variant == p.Variant {
			return m.Digest
		}
		if fallback == "" {
			fallback = m.Digest
		}
	}
	return fallback
}

// parseChallenge parses a WWW-Authenticate header into its scheme and
// comma-separated key="value" parameters.
func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil
	}
	scheme, rest, _ := strings.Cut(header, " ")
	params := map[string]string{}
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		after = strings.TrimSpace(after)
		var value string
		if strings.HasPrefix(after, `"`) {
			after = after[1:]
			var sb strings.Builder
			escaped := false
			i := 0
			for ; i < len(after); i++ {
				ch := after[i]
				if escaped {
					sb.WriteByte(ch)
					escaped = false
					continue
				}
				if ch == '\\' {
					escaped = true
					continue
				}
				if ch == '"' {
					i++
					break
				}
				sb.WriteByte(ch)
			}
			value = sb.String()
			rest = after[i:]
		} else {
			value, rest, _ = strings.Cut(after, ",")
			value = strings.TrimSpace(value)
		}
		if key != "" {
			params[strings.ToLower(key)] = value
		}
	}
	return scheme, params
}

// loadDockerConfig reads credentials from Docker's config.json. Credential
// helpers (credsStore) are intentionally ignored: invoking arbitrary helper
// binaries from a long-running daemon is a security risk best left to the
// operator, who can pass credentials through drydock's own config.
func loadDockerConfig() map[string]Credentials {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, ".docker")
	}
	path := filepath.Join(dir, "config.json")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var raw struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return nil
	}

	out := make(map[string]Credentials, len(raw.Auths))
	for host, a := range raw.Auths {
		creds := Credentials{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
		if a.Auth != "" && creds.Username == "" {
			if decoded, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				if u, p, ok := strings.Cut(string(decoded), ":"); ok {
					creds.Username, creds.Password = u, p
				}
			}
		}
		out[host] = creds
	}
	return out
}

type retryableError struct {
	status int
	after  time.Duration
}

func (e *retryableError) Error() string {
	return fmt.Sprintf("transient registry failure: status %d", e.status)
}

func sleepBackoff(ctx context.Context, attempt int, lastErr error) error {
	delay := time.Duration(1<<min(attempt, 5)) * 200 * time.Millisecond
	var re *retryableError
	if errors.As(lastErr, &re) && re.after > 0 {
		delay = re.after
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
