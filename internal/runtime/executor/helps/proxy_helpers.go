package helps

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use the execution-scoped request proxy if configured (highest priority)
// 2. Use auth.ProxyURL if configured
// 3. Use cfg.ProxyURL if auth proxy is not configured
// 4. Use RoundTripper from context if none are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// Priority: request override, then auth.ProxyURL, then cfg.ProxyURL.
	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := buildProxyTransport(proxyURL)
		if transport != nil {
			httpClient.Transport = transport
		} else {
			// If proxy setup failed, log and fall through to context RoundTripper
			log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
			if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
				httpClient.Transport = rt
			}
		}
	} else if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
		httpClient.Transport = rt
	}

	applyUpstreamResponseHeaderTimeout(httpClient, cfg)

	return httpClient
}

// applyUpstreamResponseHeaderTimeout bounds how long the client waits for
// upstream response headers.
//
// Without this cap, a request to an upstream that accepts the TCP connection but
// never writes a response parks the calling goroutine until the OS TCP stack
// exhausts its retransmissions, which can take tens of minutes. Only the wait for
// headers is bounded; streaming after the headers arrive is unaffected.
//
// Custom RoundTrippers (for example the uTLS transports used for browser
// fingerprint parity) cannot carry this cap: http.Transport.ResponseHeaderTimeout
// has no equivalent on the RoundTripper interface, so those clients keep their
// previous behaviour.
func applyUpstreamResponseHeaderTimeout(client *http.Client, cfg *config.Config) {
	if client == nil {
		return
	}
	seconds := cfg.UpstreamResponseHeaderTimeoutSeconds()
	if seconds <= 0 {
		return
	}
	headerTimeout := time.Duration(seconds) * time.Second

	switch transport := client.Transport.(type) {
	case nil:
		// A nil Transport makes net/http fall back to the process-wide
		// http.DefaultTransport, which has no cap. Use a shared capped transport
		// so the cap stays local to this client instead of mutating global state,
		// and so callers that derive their own connection pools from the base
		// transport keep reusing one pool instead of a fresh clone per request.
		client.Transport = sharedUpstreamTimeoutTransport(headerTimeout)
	case *http.Transport:
		if transport == nil {
			// A typed-nil *http.Transport satisfies the interface nil check above
			// but panics on dereference. Substitute the shared capped transport.
			client.Transport = sharedUpstreamTimeoutTransport(headerTimeout)
			return
		}
		// buildProxyTransport returns a freshly built transport per call, so
		// setting the field here does not affect other clients.
		transport.ResponseHeaderTimeout = headerTimeout
	default:
		log.Debugf("upstream response header timeout not applied: custom RoundTripper %T", transport)
	}
}

// upstreamTimeoutTransports caches one capped transport per timeout value. Clients
// without an explicit transport would otherwise clone http.DefaultTransport on
// every request, and every clone is a separate connection pool, so each request
// pays a fresh TCP + TLS handshake instead of reusing keep-alive connections.
var upstreamTimeoutTransports sync.Map // time.Duration -> *http.Transport

// sharedUpstreamTimeoutTransport returns the cached transport carrying the given
// response-header timeout, creating it from the process default on first use.
func sharedUpstreamTimeoutTransport(headerTimeout time.Duration) *http.Transport {
	if cached, ok := upstreamTimeoutTransports.Load(headerTimeout); ok {
		if transport, okTransport := cached.(*http.Transport); okTransport && transport != nil {
			return transport
		}
	}
	var created *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok && base != nil {
		created = base.Clone()
	} else {
		created = &http.Transport{}
	}
	created.ResponseHeaderTimeout = headerTimeout
	actual, _ := upstreamTimeoutTransports.LoadOrStore(headerTimeout, created)
	if transport, okTransport := actual.(*http.Transport); okTransport && transport != nil {
		return transport
	}
	return created
}

var devinTransportCache = NewTransportCache[string](DefaultTransportCacheCapacity)

// NewDevinHTTPClient creates an HTTP client customized for Devin Connect-RPC upstream.
// Suppresses automatic Accept-Encoding: gzip while preserving connection reuse across requests.
func NewDevinHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	// A request proxy replaces both the injected round tripper and credential/global proxy.
	// Respect explicitly injected context RoundTripper only when no request override is set.
	if cliproxyexecutor.RequestProxyURL(ctx) == "" && ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			if tr, ok := rt.(*http.Transport); ok {
				key := fmt.Sprintf("rt:%p", tr)
				cloned, err := devinTransportCache.Get(key, func() (*http.Transport, error) {
					c := tr.Clone()
					c.DisableCompression = true
					return c, nil
				})
				if err == nil && cloned != nil {
					return &http.Client{
						Transport: cloned,
						Timeout:   timeout,
					}
				}
			}
			return &http.Client{
				Transport: devinNoGzipRoundTripper{base: rt},
				Timeout:   timeout,
			}
		}
	}

	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	tr, err := devinTransportCache.Get(proxyURL, func() (*http.Transport, error) {
		var base *http.Transport
		if proxyURL != "" {
			base = buildProxyTransport(proxyURL)
		}
		if base == nil {
			if dt, ok := http.DefaultTransport.(*http.Transport); ok {
				base = dt.Clone()
			} else {
				base = &http.Transport{}
			}
		}
		base.DisableCompression = true
		return base, nil
	})
	if err != nil || tr == nil {
		tr = &http.Transport{DisableCompression: true}
	}

	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

type devinNoGzipRoundTripper struct {
	base http.RoundTripper
}

func (rt devinNoGzipRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	return rt.base.RoundTrip(req)
}

func effectiveProxyURL(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	if proxyURL := cliproxyexecutor.RequestProxyURL(ctx); proxyURL != "" {
		return proxyURL
	}
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
