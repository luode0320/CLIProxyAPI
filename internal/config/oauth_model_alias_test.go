package config

import "testing"

func TestSanitizeOAuthModelAlias_PreservesOptionalFields(t *testing.T) {
	cfg := &Config{
		OAuthModelAlias: map[string][]OAuthModelAlias{
			" CoDeX ": {
				{Name: " gpt-5 ", Alias: " g5 ", Fork: true, DisplayName: " GPT Five ", ForceMapping: true},
				{Name: "gpt-6", Alias: "g6"},
			},
		},
	}

	cfg.SanitizeOAuthModelAlias()

	aliases := cfg.OAuthModelAlias["codex"]
	if len(aliases) != 2 {
		t.Fatalf("expected 2 sanitized aliases, got %d", len(aliases))
	}
	if aliases[0].Name != "gpt-5" || aliases[0].Alias != "g5" || !aliases[0].Fork || aliases[0].DisplayName != "GPT Five" || !aliases[0].ForceMapping {
		t.Fatalf("unexpected sanitized first alias: %+v", aliases[0])
	}
	if aliases[1].Name != "gpt-6" || aliases[1].Alias != "g6" || aliases[1].Fork || aliases[1].DisplayName != "" || aliases[1].ForceMapping {
		t.Fatalf("unexpected sanitized second alias: %+v", aliases[1])
	}
}

func TestSanitizeOAuthModelAlias_AllowsMultipleAliasesForSameName(t *testing.T) {
	cfg := &Config{
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"antigravity": {
				{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101", Fork: true},
				{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101-thinking", Fork: true},
				{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5", Fork: true},
			},
		},
	}

	cfg.SanitizeOAuthModelAlias()

	aliases := cfg.OAuthModelAlias["antigravity"]
	expected := []OAuthModelAlias{
		{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101", Fork: true},
		{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5-20251101-thinking", Fork: true},
		{Name: "gemini-claude-opus-4-5-thinking", Alias: "claude-opus-4-5", Fork: true},
	}
	if len(aliases) != len(expected) {
		t.Fatalf("expected %d sanitized aliases, got %d", len(expected), len(aliases))
	}
	for i, exp := range expected {
		if aliases[i].Name != exp.Name || aliases[i].Alias != exp.Alias || aliases[i].Fork != exp.Fork {
			t.Fatalf("expected alias %d to be name=%q alias=%q fork=%v, got name=%q alias=%q fork=%v", i, exp.Name, exp.Alias, exp.Fork, aliases[i].Name, aliases[i].Alias, aliases[i].Fork)
		}
	}
}

func TestSanitizeOAuthModelAlias_SupportsCaseSensitiveAliases(t *testing.T) {
	cfg := &Config{
		OAuthModelAlias: map[string][]OAuthModelAlias{
			"traework-provider": {
				// Case-only mapping must be kept so clients can request the
				// lower-cased alias while the upstream receives the exact name.
				{Name: "DeepSeek-V4-Pro", Alias: "deepseek-v4-pro"},
				// Exact name==alias stays a no-op and is dropped.
				{Name: "gpt-5", Alias: "gpt-5"},
				// Exact duplicate alias is dropped; case-variant duplicates are kept.
				{Name: "model-a", Alias: "shared"},
				{Name: "model-b", Alias: "shared"},
				{Name: "model-c", Alias: "Shared"},
			},
		},
	}

	cfg.SanitizeOAuthModelAlias()

	aliases := cfg.OAuthModelAlias["traework-provider"]
	if len(aliases) != 3 {
		t.Fatalf("expected 3 sanitized aliases, got %d: %+v", len(aliases), aliases)
	}
	if aliases[0].Name != "DeepSeek-V4-Pro" || aliases[0].Alias != "deepseek-v4-pro" {
		t.Fatalf("expected case-only alias to be preserved, got name=%q alias=%q", aliases[0].Name, aliases[0].Alias)
	}
	// The exact duplicate alias ("model-b" -> "shared") is dropped, while the
	// case-variant alias ("model-c" -> "Shared") is kept.
	if aliases[1].Name != "model-a" || aliases[1].Alias != "shared" {
		t.Fatalf("expected first shared alias from model-a, got name=%q alias=%q", aliases[1].Name, aliases[1].Alias)
	}
	if aliases[2].Name != "model-c" || aliases[2].Alias != "Shared" {
		t.Fatalf("expected case-variant alias from model-c to be kept, got name=%q alias=%q", aliases[2].Name, aliases[2].Alias)
	}
}
