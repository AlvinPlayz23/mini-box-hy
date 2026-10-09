package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dockerHubRegistry is the host that serves Docker Hub content; the
// docker.io name is only used for auth and reference parsing.
const dockerHubRegistry = "registry-1.docker.io"

// Client is a minimal OCI distribution (registry v2) client supporting
// anonymous bearer-token pulls and optional basic auth.
type Client struct {
	host     string
	base     string
	username string
	password string
	http     *http.Client

	mu     sync.Mutex
	tokens map[string]string
}

// NewClient builds a client for a registry host such as docker.io or ghcr.io.
func NewClient(registry string) *Client {
	host := registry
	if host == "" || host == DefaultRegistry {
		host = dockerHubRegistry
	}
	scheme := "https"
	if isLocalRegistry(host) {
		scheme = "http"
	}
	return &Client{
		host:     host,
		base:     scheme + "://" + host + "/v2/",
		username: os.Getenv("MINIBOX_REGISTRY_USERNAME"),
		password: os.Getenv("MINIBOX_REGISTRY_PASSWORD"),
		http:     &http.Client{Timeout: 5 * time.Minute},
		tokens:   map[string]string{},
	}
}

func isLocalRegistry(host string) bool {
	h := host
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".local")
}

// Host returns the registry host this client talks to.
func (c *Client) Host() string { return c.host }

// do performs a request with bearer auth, fetching a token when the registry
// challenges with a WWW-Authenticate header.
func (c *Client) do(ctx context.Context, method, path, scope string, accept []string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	if token := c.cachedToken(scope); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request %s %s: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer") {
		return nil, fmt.Errorf("registry %s requires authentication (%s) and minibox only supports bearer tokens; "+
			"set MINIBOX_REGISTRY_USERNAME and MINIBOX_REGISTRY_PASSWORD for private registries", c.host, challenge)
	}
	params := parseAuthChallenge(challenge)
	token, err := c.fetchToken(ctx, params, scope)
	if err != nil {
		return nil, err
	}
	req2, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	for _, a := range accept {
		req2.Header.Add("Accept", a)
	}
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := c.http.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("registry request %s %s: %w", method, path, err)
	}
	return resp2, nil
}

func (c *Client) cachedToken(scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[scope]
}

func (c *Client) storeToken(scope, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[scope] = token
}

// parseAuthChallenge extracts the realm, service and scope of a bearer
// challenge.
func parseAuthChallenge(challenge string) map[string]string {
	out := map[string]string{}
	rest := strings.TrimSpace(strings.TrimPrefix(challenge, "Bearer"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "bearer"))
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		out[key] = val
	}
	return out
}

// fetchToken exchanges a bearer challenge for a token.
func (c *Client) fetchToken(ctx context.Context, params map[string]string, scope string) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry %s sent a bearer challenge without a realm", c.host)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse auth realm %q: %w", realm, err)
	}
	q := u.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	} else if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch registry token from %s: %w", realm, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint %s returned %s; "+
			"for private images set MINIBOX_REGISTRY_USERNAME and MINIBOX_REGISTRY_PASSWORD", realm, resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("registry token endpoint %s returned no token", realm)
	}
	c.storeToken(scope, token)
	return token, nil
}

// FetchManifest fetches a manifest or index by tag or digest. It returns the
// raw bytes, the media type and the digest computed from the body.
func (c *Client) FetchManifest(ctx context.Context, repo, reference string) ([]byte, string, string, error) {
	scope := "repository:" + repo + ":pull"
	accept := []string{
		MediaTypeOCIIndex, MediaTypeDockerManifestList,
		MediaTypeOCIManifest, MediaTypeDockerManifest,
	}
	resp, err := c.do(ctx, http.MethodGet, repo+"/manifests/"+reference, scope, accept)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", describeStatus("manifest", repo, reference, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", "", fmt.Errorf("read manifest %s: %w", reference, err)
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if header := resp.Header.Get("Docker-Content-Digest"); header != "" && header != digest {
		return nil, "", "", fmt.Errorf("manifest digest mismatch for %s: registry says %s but the body hashes to %s", reference, header, digest)
	}
	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = MediaTypeOCIManifest
	}
	return body, mediaType, digest, nil
}

// ResolveManifest resolves a reference to a single-platform image manifest,
// following an index when necessary.
func (c *Client) ResolveManifest(ctx context.Context, ref Reference, want Platform) (*Manifest, string, error) {
	reference := ref.Identifier()
	body, mediaType, digest, err := c.FetchManifest(ctx, ref.Repository, reference)
	if err != nil {
		return nil, "", err
	}
	if isIndexMediaType(mediaType) || looksLikeIndex(body) {
		var idx Index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, "", fmt.Errorf("parse manifest list for %s: %w", ref.Familiar(), err)
		}
		desc, err := selectPlatform(idx.Manifests, want)
		if err != nil {
			return nil, "", err
		}
		body, mediaType, digest, err = c.FetchManifest(ctx, ref.Repository, desc.Digest)
		if err != nil {
			return nil, "", err
		}
		if desc.Digest != digest {
			return nil, "", fmt.Errorf("platform manifest digest mismatch: index lists %s, body hashes to %s", desc.Digest, digest)
		}
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, "", fmt.Errorf("parse manifest for %s (media type %s): %w", ref.Familiar(), mediaType, err)
	}
	if manifest.Config.Digest == "" || len(manifest.Layers) == 0 {
		return nil, "", fmt.Errorf("manifest for %s has no config or layers", ref.Familiar())
	}
	return &manifest, digest, nil
}

func isIndexMediaType(mediaType string) bool {
	return mediaType == MediaTypeOCIIndex || mediaType == MediaTypeDockerManifestList
}

func looksLikeIndex(body []byte) bool {
	var probe struct {
		Manifests []json.RawMessage `json:"manifests"`
		Layers    []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return len(probe.Manifests) > 0 && len(probe.Layers) == 0
}

// selectPlatform picks the descriptor matching want.
func selectPlatform(descriptors []Descriptor, want Platform) (Descriptor, error) {
	var available []string
	for _, d := range descriptors {
		if d.Platform == nil {
			continue
		}
		available = append(available, d.Platform.String())
		if d.Platform.Matches(want) {
			return d, nil
		}
	}
	if want.Architecture == "arm64" && want.Variant == "" {
		for _, d := range descriptors {
			if d.Platform != nil && d.Platform.OS == want.OS && d.Platform.Architecture == "arm64" {
				return d, nil
			}
		}
	}
	return Descriptor{}, fmt.Errorf("image has no %s variant (available: %s); "+
		"use --platform to pick one of them", want, strings.Join(available, ", "))
}

// FetchBlob returns a reader for a blob. The caller must close it.
func (c *Client) FetchBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error) {
	scope := "repository:" + repo + ":pull"
	resp, err := c.do(ctx, http.MethodGet, repo+"/blobs/"+digest, scope, nil)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, describeStatus("blob", repo, digest, resp)
	}
	return resp.Body, resp.ContentLength, nil
}

func describeStatus(kind, repo, reference string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("registry %s refused access to %s %s in %s (HTTP 401): "+
			"the image may not exist, or it is private and needs "+
			"MINIBOX_REGISTRY_USERNAME and MINIBOX_REGISTRY_PASSWORD", resp.Request.URL.Host, kind, reference, repo)
	case http.StatusNotFound:
		return fmt.Errorf("%s %s not found in %s (HTTP 404): check the image name and tag", kind, reference, repo)
	case http.StatusTooManyRequests:
		retry := resp.Header.Get("Retry-After")
		if retry == "" {
			retry = "a few"
		}
		return fmt.Errorf("registry rate limit reached for %s: retry in %s seconds (Docker Hub limits anonymous pulls)", repo, retry)
	default:
		return fmt.Errorf("registry returned %s for %s %s in %s", resp.Status, kind, reference, repo)
	}
}

// DownloadBlob streams a blob to path, verifying its digest as it goes. The
// returned size is the number of bytes written.
func (c *Client) DownloadBlob(ctx context.Context, repo, digest, path string) (int64, error) {
	d, err := ParseDigest(digest)
	if err != nil {
		return 0, err
	}
	body, _, err := c.FetchBlob(ctx, repo, digest)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create blob file %s: %w", path, err)
	}
	defer f.Close()

	verifier, err := NewVerifier(d)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(io.MultiWriter(f, verifier), body)
	if err != nil {
		return n, fmt.Errorf("download blob %s: %w", digest, err)
	}
	if err := verifier.Close(); err != nil {
		return n, fmt.Errorf("blob %s is corrupt: %w", digest, err)
	}
	return n, nil
}

// ParseContentLength is a helper for tests and diagnostics.
func ParseContentLength(v string) int64 {
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}
