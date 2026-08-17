package pluginstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectReleaseAssets(t *testing.T) {
	t.Parallel()

	release := Release{Assets: []ReleaseAsset{
		{Name: "sample-provider_0.1.0_darwin_arm64.zip", BrowserDownloadURL: "https://example.com/sample-provider.zip"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums.txt"},
	}}
	archiveAsset, checksumAsset, errSelect := SelectReleaseAssets(release, "sample-provider", "0.1.0", "darwin", "arm64")
	if errSelect != nil {
		t.Fatalf("SelectReleaseAssets() error = %v", errSelect)
	}
	if archiveAsset.BrowserDownloadURL != "https://example.com/sample-provider.zip" {
		t.Fatalf("archive URL = %q", archiveAsset.BrowserDownloadURL)
	}
	if checksumAsset.BrowserDownloadURL != "https://example.com/checksums.txt" {
		t.Fatalf("checksum URL = %q", checksumAsset.BrowserDownloadURL)
	}
}

func TestSelectReleaseAssetsRejectsMissingAssets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		release Release
		wantErr string
	}{
		{
			name: "missing zip",
			release: Release{Assets: []ReleaseAsset{
				{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums.txt"},
			}},
			wantErr: "sample-provider_0.1.0_darwin_arm64.zip",
		},
		{
			name: "missing checksum",
			release: Release{Assets: []ReleaseAsset{
				{Name: "sample-provider_0.1.0_darwin_arm64.zip", BrowserDownloadURL: "https://example.com/sample-provider.zip"},
			}},
			wantErr: "checksums.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, errSelect := SelectReleaseAssets(tt.release, "sample-provider", "0.1.0", "darwin", "arm64")
			if errSelect == nil {
				t.Fatal("SelectReleaseAssets() error = nil")
			}
			if !strings.Contains(errSelect.Error(), tt.wantErr) {
				t.Fatalf("SelectReleaseAssets() error = %v, want substring %q", errSelect, tt.wantErr)
			}
		})
	}
}

func TestReleaseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tagName string
		want    string
		wantErr bool
	}{
		{name: "v prefix", tagName: "v1.2.3", want: "1.2.3"},
		{name: "no prefix", tagName: "0.1.0", want: "0.1.0"},
		{name: "whitespace", tagName: " v2.0.0 ", want: "2.0.0"},
		{name: "empty", tagName: "", wantErr: true},
		{name: "non numeric", tagName: "latest", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, errVersion := ReleaseVersion(Release{TagName: tt.tagName})
			if tt.wantErr {
				if errVersion == nil {
					t.Fatalf("ReleaseVersion(%q) error = nil", tt.tagName)
				}
				return
			}
			if errVersion != nil {
				t.Fatalf("ReleaseVersion(%q) error = %v", tt.tagName, errVersion)
			}
			if version != tt.want {
				t.Fatalf("ReleaseVersion(%q) = %q, want %q", tt.tagName, version, tt.want)
			}
		})
	}
}

func TestParseChecksumsAndVerifyChecksum(t *testing.T) {
	t.Parallel()

	data := []byte("zip-data")
	sum := sha256.Sum256(data)
	checksumText := hex.EncodeToString(sum[:]) + "  sample-provider_0.1.0_darwin_arm64.zip\n"
	checksums, errParse := ParseChecksums([]byte(checksumText))
	if errParse != nil {
		t.Fatalf("ParseChecksums() error = %v", errParse)
	}
	if errVerify := VerifyChecksum("sample-provider_0.1.0_darwin_arm64.zip", data, checksums); errVerify != nil {
		t.Fatalf("VerifyChecksum() error = %v", errVerify)
	}
}

func TestVerifyChecksumRejectsMissingAndMismatch(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("zip-data"))
	checksums := map[string]string{"sample-provider.zip": hex.EncodeToString(sum[:])}
	if errVerify := VerifyChecksum("missing.zip", []byte("zip-data"), checksums); errVerify == nil {
		t.Fatal("VerifyChecksum() missing checksum error = nil")
	}
	if errVerify := VerifyChecksum("sample-provider.zip", []byte("other"), checksums); errVerify == nil {
		t.Fatal("VerifyChecksum() mismatch error = nil")
	}
}

func TestClientGetCachesSuccessfulResponse(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := Client{
		HTTPClient: server.Client(),
		Cache:      &ClientCache{},
	}
	for call := 0; call < 2; call++ {
		data, errGet := client.get(context.Background(), server.URL, "application/json", RequestKindRegistry, 0)
		if errGet != nil {
			t.Fatalf("get() error = %v", errGet)
		}
		if string(data) != `{"ok":true}` {
			t.Fatalf("get() body = %q", string(data))
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1 (cached)", calls.Load())
	}
}

func TestClientGetBacksOffOnTooManyRequests(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "429: Too Many Requests For more on scraping GitHub", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := Client{
		HTTPClient: server.Client(),
		Cache:      &ClientCache{},
	}
	first, errFirst := client.get(context.Background(), server.URL, "application/json", RequestKindRegistry, 0)
	if errFirst == nil {
		t.Fatal("get() error = nil, want rate limit error")
	}
	if first != nil {
		t.Fatalf("get() data = %q, want nil", string(first))
	}
	if !strings.Contains(errFirst.Error(), "Too Many Requests") {
		t.Fatalf("get() error = %v, want 429 body", errFirst)
	}

	second, errSecond := client.get(context.Background(), server.URL, "application/json", RequestKindRegistry, 0)
	if second != nil {
		t.Fatalf("get() second data = %q, want nil", string(second))
	}
	if errSecond == nil || errSecond.Error() != errFirst.Error() {
		t.Fatalf("get() second error = %v, want cached first error %v", errSecond, errFirst)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1 (backoff suppressed retry)", calls.Load())
	}
}

func TestClientGetConcurrentRequestsCoalesce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := Client{
		HTTPClient: server.Client(),
		Cache:      &ClientCache{},
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, errGet := client.get(ctx, server.URL, "application/json", RequestKindRegistry, 0)
			if errGet != nil {
				t.Errorf("get() error = %v", errGet)
			}
			if string(data) != `{"ok":true}` {
				t.Errorf("get() body = %q", string(data))
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1 (coalesced)", calls.Load())
	}
}

func TestClientGetDoesNotCacheArtifactDownloads(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("plugin-binary"))
	}))
	defer server.Close()

	client := Client{
		HTTPClient: server.Client(),
		Cache:      &ClientCache{},
	}
	for call := 0; call < 2; call++ {
		if _, errGet := client.get(context.Background(), server.URL, "application/octet-stream", RequestKindArtifact, 0); errGet != nil {
			t.Fatalf("get() error = %v", errGet)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream called %d times, want 2 (artifacts not cached)", calls.Load())
	}
}

func TestRetryAfterDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "seconds", retryAfter: "60", want: 60 * time.Second},
		{name: "zero", retryAfter: "0", want: 0},
		{name: "empty", retryAfter: "", want: pluginStoreFailureCacheTTL},
		{name: "invalid", retryAfter: "soon", want: pluginStoreFailureCacheTTL},
		{name: "capped", retryAfter: "3600", want: pluginStoreBackoffMaxTTL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := retryAfterDelay(tt.retryAfter); got != tt.want {
				t.Fatalf("retryAfterDelay(%q) = %v, want %v", tt.retryAfter, got, tt.want)
			}
		})
	}
}
