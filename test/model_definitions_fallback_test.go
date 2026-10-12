package test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// modelDefinitionsResponse mirrors the handler's JSON payload for assertions.
type modelDefinitionsResponse struct {
	Channel string `json:"channel"`
	Models  []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
	} `json:"models"`
}

func callGetStaticModelDefinitions(t *testing.T, channel string) (int, modelDefinitionsResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Params = gin.Params{{Key: "channel", Value: channel}}
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/model-definitions/"+channel, nil)

	h := management.NewHandlerWithoutConfigFilePath(nil, nil)
	h.GetStaticModelDefinitions(ginCtx)

	var payload modelDefinitionsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
		}
	}
	return rec.Code, payload
}

// Static channels must keep returning the embedded catalog and never consult
// the dynamic registry, so existing management behavior is unchanged.
func TestGetStaticModelDefinitions_StaticChannelUnchanged(t *testing.T) {
	code, payload := callGetStaticModelDefinitions(t, "claude")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d", code, http.StatusOK)
	}
	if len(payload.Models) == 0 {
		t.Fatal("static channel returned no models")
	}
	if payload.Channel != "claude" {
		t.Fatalf("channel = %q, want %q", payload.Channel, "claude")
	}
}

// A provider with no static definition but registered models (e.g. a plugin
// provider) must fall back to the dynamic registry instead of 400.
func TestGetStaticModelDefinitions_FallsBackToRegistryForPluginProvider(t *testing.T) {
	const provider = "plugin-fallback-test-provider"

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("plugin:"+provider+":executor", provider, []*registry.ModelInfo{
		{ID: "qwen3.8-flash", DisplayName: "Qwen3.8 Flash", Type: provider},
		{ID: "auto", DisplayName: "Auto", Type: provider},
	})
	defer reg.UnregisterClient("plugin:" + provider + ":executor")

	code, payload := callGetStaticModelDefinitions(t, provider)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body must expose plugin models)", code, http.StatusOK)
	}
	if len(payload.Models) == 0 {
		t.Fatal("fallback returned no models for a registered plugin provider")
	}

	ids := make(map[string]struct{}, len(payload.Models))
	for _, m := range payload.Models {
		ids[m.ID] = struct{}{}
	}
	for _, want := range []string{"qwen3.8-flash", "auto"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("fallback response missing model %q, got %#v", want, ids)
		}
	}
}

// A channel with neither a static definition nor registered models must keep
// returning 400 so the management UI still reports "unsupported".
func TestGetStaticModelDefinitions_UnknownChannelStill400(t *testing.T) {
	const channel = "definitely-unknown-channel-xyz"

	code, _ := callGetStaticModelDefinitions(t, channel)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", code, http.StatusBadRequest)
	}
}
