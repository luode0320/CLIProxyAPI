package helps

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
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

	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

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
		// http.DefaultTransport. Clone it first so the cap stays local to this
		// client instead of mutating shared global state.
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			log.Debugf("upstream response header timeout not applied: global default transport is %T", http.DefaultTransport)
			return
		}
		cloned := base.Clone()
		cloned.ResponseHeaderTimeout = headerTimeout
		client.Transport = cloned
	case *http.Transport:
		// buildProxyTransport returns a freshly built transport per call, so
		// setting the field here does not affect other clients.
		transport.ResponseHeaderTimeout = headerTimeout
	default:
		log.Debugf("upstream response header timeout not applied: custom RoundTripper %T", transport)
	}
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
