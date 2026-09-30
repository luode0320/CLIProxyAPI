package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestNewProxyAwareHTTPClientRequestProxyOverridesAuthAndGlobal(t *testing.T) {
	t.Parallel()

	ctx := coreexecutor.WithRequestProxyURL(context.Background(), "http://request-proxy.example:8081")
	client := NewProxyAwareHTTPClient(
		ctx,
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"},
		0,
	)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("transport = %#v, want request proxy", client.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://upstream.example/v1", nil)
	if errReq != nil {
		t.Fatalf("request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://request-proxy.example:8081" {
		t.Fatalf("proxy URL = %v, want request proxy", proxyURL)
	}

	refreshCtx := coreexecutor.WithoutRequestProxyURL(ctx)
	refreshClient := NewProxyAwareHTTPClient(
		refreshCtx,
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"},
		0,
	)
	refreshTransport, ok := refreshClient.Transport.(*http.Transport)
	if !ok || refreshTransport.Proxy == nil {
		t.Fatalf("refresh transport = %#v, want auth proxy", refreshClient.Transport)
	}
	refreshProxy, errRefresh := refreshTransport.Proxy(req)
	if errRefresh != nil {
		t.Fatalf("refresh proxy: %v", errRefresh)
	}
	if refreshProxy == nil || refreshProxy.String() != "http://auth-proxy.example:8080" {
		t.Fatalf("refresh proxy URL = %v, want auth proxy", refreshProxy)
	}
}

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientAppliesDefaultUpstreamResponseHeaderTimeout(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	want := time.Duration(config.DefaultUpstreamResponseHeaderTimeout) * time.Second
	if transport.ResponseHeaderTimeout != want {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, want)
	}
}

func TestNewProxyAwareHTTPClientDoesNotMutateGlobalDefaultTransport(t *testing.T) {
	t.Parallel()

	global, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not *http.Transport")
	}
	before := global.ResponseHeaderTimeout

	client := NewProxyAwareHTTPClient(context.Background(), &config.Config{}, nil, 0)
	if _, ok := client.Transport.(*http.Transport); !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}

	// The cap must land on a clone: mutating the process-wide default transport
	// would silently retune every other HTTP call in the process.
	if global.ResponseHeaderTimeout != before {
		t.Fatalf("global http.DefaultTransport mutated: %v -> %v", before, global.ResponseHeaderTimeout)
	}
	if client.Transport == http.DefaultTransport {
		t.Fatal("client shares the process-wide default transport instead of a clone")
	}
}

func TestNewProxyAwareHTTPClientHonoursCustomUpstreamResponseHeaderTimeout(t *testing.T) {
	t.Parallel()

	custom := 45
	client := NewProxyAwareHTTPClient(context.Background(), &config.Config{
		UpstreamResponseHeaderTimeout: &custom,
	}, nil, 0)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if want := 45 * time.Second; transport.ResponseHeaderTimeout != want {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, want)
	}
}

func TestNewProxyAwareHTTPClientDisablesUpstreamResponseHeaderTimeout(t *testing.T) {
	t.Parallel()

	disabled := 0
	client := NewProxyAwareHTTPClient(context.Background(), &config.Config{
		UpstreamResponseHeaderTimeout: &disabled,
	}, nil, 0)

	// Disabling the cap must leave the client untouched, so net/http keeps
	// falling back to its own default transport: the previous behaviour.
	if client.Transport != nil {
		t.Fatalf("transport = %#v, want nil (cap disabled, behaviour unchanged)", client.Transport)
	}
}

func TestNewProxyAwareHTTPClientAppliesTimeoutToProxyTransport(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://proxy.example.com:8080"}},
		nil,
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy function to be configured")
	}
	want := time.Duration(config.DefaultUpstreamResponseHeaderTimeout) * time.Second
	if transport.ResponseHeaderTimeout != want {
		t.Fatalf("ResponseHeaderTimeout = %v, want %v", transport.ResponseHeaderTimeout, want)
	}
}

func TestNewDevinHTTPClient_ReusesTransportFromContext(t *testing.T) {
	baseTransport := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)

	c1 := NewDevinHTTPClient(ctx, nil, nil, 0)
	c2 := NewDevinHTTPClient(ctx, nil, nil, 0)

	if c1.Transport != c2.Transport {
		t.Errorf("expected c1.Transport == c2.Transport across requests, got different pointers %p vs %p", c1.Transport, c2.Transport)
	}

	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c1.Transport)
	}
	if !tr.DisableCompression {
		t.Error("expected DisableCompression = true")
	}
}

func TestNewDevinHTTPClient_NonStandardRoundTripperDisablesGzip(t *testing.T) {
	var seenEncoding string
	customRT := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seenEncoding = req.Header.Get("Accept-Encoding")
		return &http.Response{StatusCode: 200}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", customRT)

	c := NewDevinHTTPClient(ctx, nil, nil, 0)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	_, _ = c.Transport.RoundTrip(req)

	if seenEncoding != "identity" {
		t.Errorf("expected Accept-Encoding: identity, got %q", seenEncoding)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
