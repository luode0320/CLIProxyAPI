package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDiffOAuthModelAliasChanges_IncludesDisplayName(t *testing.T) {
	oldMap := map[string][]config.OAuthModelAlias{
		"antigravity": {
			{Name: "claude-opus-4-6-thinking", Alias: "claude-antigravity-opus-4-6-thinking", DisplayName: "Antigravity Opus 4.6"},
		},
	}
	newMap := map[string][]config.OAuthModelAlias{
		"antigravity": {
			{Name: "claude-opus-4-6-thinking", Alias: "claude-antigravity-opus-4-6-thinking", DisplayName: "Antigravity Opus 4.6 (Thinking)"},
		},
	}

	changes, affected := DiffOAuthModelAliasChanges(oldMap, newMap)
	expectContains(t, changes, "oauth-model-alias[antigravity]: updated (1 -> 1 entries)")
	if len(affected) != 1 || affected[0] != "antigravity" {
		t.Fatalf("expected antigravity to be affected, got %#v", affected)
	}
}

func TestDiffOAuthModelAliasChanges_CaseOnlyChangeIsDetected(t *testing.T) {
	oldMap := map[string][]config.OAuthModelAlias{
		"traework-provider": {
			{Name: "DeepSeek-V4-Pro", Alias: "deepseek-v4-pro"},
		},
	}
	newMap := map[string][]config.OAuthModelAlias{
		"traework-provider": {
			{Name: "deepseek-v4-pro", Alias: "DeepSeek-V4-Pro"},
		},
	}

	changes, affected := DiffOAuthModelAliasChanges(oldMap, newMap)
	expectContains(t, changes, "oauth-model-alias[traework-provider]: updated (1 -> 1 entries)")
	if len(affected) != 1 || affected[0] != "traework-provider" {
		t.Fatalf("expected traework-provider to be affected, got %#v", affected)
	}
}
