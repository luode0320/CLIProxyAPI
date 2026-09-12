package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

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
