package pluginstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const userAgent = "CLIProxyAPI"
const maxPluginStoreRedirects = 10

const (
	// pluginStoreCacheTTL bounds how long a successfully fetched registry or
	// release response is reused before the upstream is queried again.
	pluginStoreCacheTTL = 10 * time.Minute
	// pluginStoreFailureCacheTTL bounds how long a rate-limited or failed fetch
	// suppresses further requests to the same URL.
	pluginStoreFailureCacheTTL = 30 * time.Second
	// pluginStoreBackoffMaxTTL caps the backoff honored from a Retry-After
	// header so a misbehaving upstream cannot stall the store indefinitely.
	pluginStoreBackoffMaxTTL = 5 * time.Minute
)

// HTTPDoer abstracts the HTTP client used to execute requests.
type HTTPDoer = httpfetch.Doer

type Client struct {
	HTTPClient            HTTPDoer
	RegistryURL           string
	UserAgent             string
	Auth                  []AuthConfig
	ResolvedAuth          []ResolvedAuthConfig
	ResolvedAuthExpiresAt time.Time
	// Cache, when nil, uses a shared package-level cache so all plugin store
	// clients (management panel, installs, home sync) coalesce onto the same
	// upstream throttle.
	Cache *ClientCache
}

// pluginStoreCacheEntry holds a cached response body or a cached fetch error.
type pluginStoreCacheEntry struct {
	value     []byte
	err       error
	expiresAt time.Time
}

// ClientCache is a process-local, URL-keyed response cache shared by plugin
// store clients. It is safe for concurrent use.
type ClientCache struct {
	mu    sync.Mutex
	items map[string]pluginStoreCacheEntry
	group singleflight.Group
}

// get returns the cached entry for key when it is present and not expired.
func (c *ClientCache) get(key string) (pluginStoreCacheEntry, bool) {
	if c == nil {
		return pluginStoreCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return pluginStoreCacheEntry{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.items, key)
		return pluginStoreCacheEntry{}, false
	}
	return entry, true
}

// set stores a cached entry for key, replacing any prior entry.
func (c *ClientCache) set(key string, entry pluginStoreCacheEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string]pluginStoreCacheEntry)
	}
	c.items[key] = entry
}

// globalPluginStoreCache is the default cache used by clients that do not set
// their own, letting separate handler and sync paths share one throttle.
var globalPluginStoreCache = &ClientCache{items: make(map[string]pluginStoreCacheEntry)}

func (c *Client) cache() *ClientCache {
	if c != nil && c.Cache != nil {
		return c.Cache
	}
	return globalPluginStoreCache
}

// cacheKey scopes cached entries by URL and request kind so an authenticated
// request never serves a response fetched by an anonymous client and vice versa.
func (c Client) cacheKey(requestURL string, kind string) string {
	authenticated := c.requestAuthenticated(requestURL, kind)
	return strings.ToLower(kind) + "|" + strconv.FormatBool(authenticated) + "|" + requestURL
}

func (c Client) requestAuthenticated(requestURL string, kind string) bool {
	if _, ok := matchingResolvedAuthConfig(c.ResolvedAuth, requestURL, kind); ok {
		return true
	}
	item, ok := matchingAuthConfig(c.Auth, requestURL, kind)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Type)) {
	case "", AuthTypeNone:
		return false
	default:
		return true
	}
}

// pluginStoreCacheable reports whether a response should be cached. Large
// artifact downloads are excluded; they are re-fetched and verified on demand.
func pluginStoreCacheable(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case RequestKindRegistry, RequestKindMetadata:
		return true
	default:
		return false
	}
}

// retryAfterDelay interprets a Retry-After header as seconds, clamped to
// [0, pluginStoreBackoffMaxTTL]. A missing or invalid header yields
// pluginStoreFailureCacheTTL.
func retryAfterDelay(retryAfter string) time.Duration {
	value := strings.TrimSpace(retryAfter)
	if value != "" {
		if seconds, errParse := strconv.Atoi(value); errParse == nil && seconds >= 0 {
			delay := time.Duration(seconds) * time.Second
			if delay > pluginStoreBackoffMaxTTL {
				delay = pluginStoreBackoffMaxTTL
			}
			return delay
		}
	}
	return pluginStoreFailureCacheTTL
}

type Release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

type ReleaseAsset struct {
	APIURL             string `json:"url"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (c Client) FetchRegistry(ctx context.Context) (Registry, error) {
	registryURL := strings.TrimSpace(c.RegistryURL)
	if registryURL == "" {
		registryURL = DefaultRegistryURL
	}
	data, errDownload := c.get(ctx, registryURL, "application/json", RequestKindRegistry, 0)
	if errDownload != nil {
		return Registry{}, errDownload
	}
	registry, errParse := ParseRegistry(data)
	if errParse != nil {
		return Registry{}, errParse
	}
	return registry, nil
}

// FetchLatestRelease returns the latest published release of the plugin's
// GitHub repository, mirroring the WebUI panel update check.
func (c Client) FetchLatestRelease(ctx context.Context, plugin Plugin) (Release, error) {
	owner, repo, errRepository := GitHubRepositoryParts(plugin.Repository)
	if errRepository != nil {
		return Release{}, errRepository
	}
	releaseURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/releases/latest",
		url.PathEscape(owner),
		url.PathEscape(repo),
	)
	data, errDownload := c.get(ctx, releaseURL, "application/vnd.github+json", RequestKindMetadata, 0)
	if errDownload != nil {
		return Release{}, errDownload
	}
	var release Release
	if errDecode := json.Unmarshal(data, &release); errDecode != nil {
		return Release{}, fmt.Errorf("decode release: %w", errDecode)
	}
	return release, nil
}

// FetchReleaseByTag returns a published release by its exact GitHub tag.
func (c Client) FetchReleaseByTag(ctx context.Context, plugin Plugin, tag string) (Release, error) {
	owner, repo, errRepository := GitHubRepositoryParts(plugin.Repository)
	if errRepository != nil {
		return Release{}, errRepository
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return Release{}, fmt.Errorf("release tag is required")
	}
	releaseURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/releases/tags/%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(tag),
	)
	data, errDownload := c.get(ctx, releaseURL, "application/vnd.github+json", RequestKindMetadata, 0)
	if errDownload != nil {
		return Release{}, errDownload
	}
	var release Release
	if errDecode := json.Unmarshal(data, &release); errDecode != nil {
		return Release{}, fmt.Errorf("decode release: %w", errDecode)
	}
	return release, nil
}

// ReleaseVersion derives the plugin version from the release tag, stripping a
// leading "v"/"V" and validating the result.
func ReleaseVersion(release Release) (string, error) {
	version := normalizeVersion(release.TagName)
	if !validPluginVersion(version) {
		return "", fmt.Errorf("invalid release tag %q", release.TagName)
	}
	return version, nil
}

func (c Client) DownloadAsset(ctx context.Context, asset ReleaseAsset) ([]byte, error) {
	downloadURL := strings.TrimSpace(asset.BrowserDownloadURL)
	apiURL := strings.TrimSpace(asset.APIURL)
	if downloadURL == "" || c.releaseAssetAPIAuthenticated(apiURL) {
		if apiURL != "" {
			downloadURL = apiURL
		}
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("asset %q missing download url", asset.Name)
	}
	return c.get(ctx, downloadURL, "application/octet-stream", RequestKindArtifact, 0)
}

func (c Client) releaseAssetAPIAuthenticated(apiURL string) bool {
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		return false
	}
	if item, ok := matchingResolvedAuthConfig(c.ResolvedAuth, apiURL, RequestKindArtifact); ok {
		return resolvedAuthConfigured(item)
	}
	return AuthConfigured(c.Auth, apiURL, RequestKindArtifact)
}

func (c Client) get(ctx context.Context, requestURL string, accept string, kind string, maxSize int64) ([]byte, error) {
	currentURL := strings.TrimSpace(requestURL)
	if currentURL == "" {
		return nil, fmt.Errorf("plugin store url is empty")
	}
	cache := c.cache()
	cacheable := pluginStoreCacheable(kind)
	key := c.cacheKey(currentURL, kind)
	if cacheable {
		if entry, ok := cache.get(key); ok {
			if entry.err != nil {
				return nil, entry.err
			}
			return entry.value, nil
		}
	}
	value, err, _ := cache.group.Do(key, func() (interface{}, error) {
		if cacheable {
			if entry, ok := cache.get(key); ok {
				if entry.err != nil {
					return nil, entry.err
				}
				return entry.value, nil
			}
		}
		return c.doGet(ctx, currentURL, accept, kind, maxSize, cacheable, key)
	})
	if err != nil {
		return nil, err
	}
	data, _ := value.([]byte)
	return data, nil
}

func (c Client) doGet(ctx context.Context, requestURL string, accept string, kind string, maxSize int64, cacheable bool, cacheKey string) ([]byte, error) {
	currentURL := strings.TrimSpace(requestURL)
	for redirects := 0; ; redirects++ {
		if errURL := validatePluginStoreRequestURL(c.Auth, currentURL, kind); errURL != nil {
			return nil, errURL
		}
		if errExpiry := validateResolvedAuthExpiry(c.ResolvedAuth, c.ResolvedAuthExpiresAt, time.Now().UTC(), currentURL, kind); errExpiry != nil {
			return nil, errExpiry
		}
		headers := http.Header{
			"Accept":     []string{accept},
			"User-Agent": []string{c.userAgent()},
		}
		authenticated, errAuth := applyPluginStoreAuthForClient(headers, c.ResolvedAuth, c.Auth, currentURL, kind)
		if errAuth != nil {
			return nil, errAuth
		}
		resp, errDo := pluginStoreGetNoRedirect(ctx, c.httpClient(), currentURL, headers)
		if authenticated {
			for name := range headers {
				headers.Del(name)
			}
			if resp != nil && resp.Request != nil {
				resp.Request.Header = nil
			}
		}
		if errDo != nil {
			return nil, errDo
		}
		if pluginStoreRedirectStatus(resp.StatusCode) {
			nextURL, errRedirect := pluginStoreRedirectURL(resp, currentURL)
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close plugin store redirect body")
			}
			if errRedirect != nil {
				return nil, errRedirect
			}
			if redirects >= maxPluginStoreRedirects {
				return nil, fmt.Errorf("stopped after %d redirects", maxPluginStoreRedirects)
			}
			currentURL = nextURL
			continue
		}
		now := time.Now()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			errStatus := pluginStoreStatusError(resp, authenticated)
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close plugin store response body")
			}
			if cacheable {
				delay := retryAfterDelay(resp.Header.Get("Retry-After"))
				c.cache().set(cacheKey, pluginStoreCacheEntry{err: errStatus, expiresAt: now.Add(delay)})
				log.WithFields(log.Fields{
					"url":          currentURL,
					"status":       resp.StatusCode,
					"backoff":      delay.String(),
					"request_kind": kind,
				}).Warn("pluginstore: upstream rate limited, backing off")
			}
			return nil, errStatus
		}
		data, errRead := readPluginStoreResponse(resp, maxSize, authenticated)
		if errRead != nil {
			return nil, errRead
		}
		if cacheable {
			c.cache().set(cacheKey, pluginStoreCacheEntry{value: data, expiresAt: now.Add(pluginStoreCacheTTL)})
		}
		return data, nil
	}
}

func (c Client) httpClient() HTTPDoer {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Client) userAgent() string {
	if strings.TrimSpace(c.UserAgent) != "" {
		return strings.TrimSpace(c.UserAgent)
	}
	return userAgent
}

func pluginStoreGetNoRedirect(ctx context.Context, client HTTPDoer, requestURL string, headers http.Header) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create request: %w", errRequest)
	}
	req.Header = headers.Clone()
	resp, errDo := pluginStoreNoRedirectClient(client).Do(req)
	if errDo != nil {
		return nil, pluginStoreRequestError(requestURL, errDo)
	}
	return resp, nil
}

func pluginStoreNoRedirectClient(client HTTPDoer) HTTPDoer {
	httpClient, ok := client.(*http.Client)
	if !ok {
		return client
	}
	clone := *httpClient
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func pluginStoreRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func pluginStoreRedirectURL(resp *http.Response, requestURL string) (string, error) {
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return "", fmt.Errorf("redirect missing Location header")
	}
	base, errBase := url.Parse(requestURL)
	if errBase != nil {
		return "", fmt.Errorf("parse redirect base: %w", errBase)
	}
	next, errNext := base.Parse(location)
	if errNext != nil {
		return "", fmt.Errorf("parse redirect location: %w", errNext)
	}
	if next.Scheme == "" || next.Host == "" {
		return "", fmt.Errorf("redirect location is not absolute")
	}
	return next.String(), nil
}

// pluginStoreStatusError builds the error reported for a non-success response.
// When the request was authenticated the response body is not read, matching
// the historical behavior that avoids exposing response content for requests
// that carried credentials.
func pluginStoreStatusError(resp *http.Response, authenticated bool) error {
	if resp == nil {
		return fmt.Errorf("unexpected empty response")
	}
	if authenticated {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func readPluginStoreResponse(resp *http.Response, maxSize int64, authenticated bool) ([]byte, error) {
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close plugin store response body")
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, pluginStoreStatusError(resp, authenticated)
	}
	reader := io.Reader(resp.Body)
	if maxSize > 0 {
		reader = io.LimitReader(resp.Body, maxSize+1)
	}
	data, errRead := io.ReadAll(reader)
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	if maxSize > 0 && int64(len(data)) > maxSize {
		return nil, fmt.Errorf("response exceeds maximum allowed size of %d bytes", maxSize)
	}
	return data, nil
}

func pluginStoreRequestError(requestURL string, err error) error {
	parsed, errParse := url.Parse(strings.TrimSpace(requestURL))
	safeURL := "plugin store url"
	if errParse == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		safeURL = parsed.String()
	}
	var urlError *url.Error
	if errors.As(err, &urlError) && urlError.Err != nil {
		err = urlError.Err
	}
	return fmt.Errorf("request %s failed: %w", safeURL, err)
}

func SelectReleaseAssets(release Release, id, version, goos, goarch string) (ReleaseAsset, ReleaseAsset, error) {
	archiveName := ArchiveName(id, version, goos, goarch)
	var archiveAsset ReleaseAsset
	var checksumAsset ReleaseAsset
	for _, asset := range release.Assets {
		switch strings.TrimSpace(asset.Name) {
		case archiveName:
			archiveAsset = asset
		case "checksums.txt":
			checksumAsset = asset
		}
	}
	if strings.TrimSpace(archiveAsset.Name) == "" {
		return ReleaseAsset{}, ReleaseAsset{}, fmt.Errorf("release asset %s not found", archiveName)
	}
	if strings.TrimSpace(checksumAsset.Name) == "" {
		return ReleaseAsset{}, ReleaseAsset{}, fmt.Errorf("release asset checksums.txt not found")
	}
	return archiveAsset, checksumAsset, nil
}

func ArchiveName(id, version, goos, goarch string) string {
	return fmt.Sprintf(
		"%s_%s_%s_%s.zip",
		strings.TrimSpace(id),
		strings.TrimSpace(version),
		strings.TrimSpace(goos),
		strings.TrimSpace(goarch),
	)
}
