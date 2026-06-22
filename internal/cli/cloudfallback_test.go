package cli

import (
	"testing"

	"github.com/orchestra-hq/atlas/internal/server"
)

// TestParseCloudFallback_valid: a well-formed spec resolves the key from the env var
// and defaults the base URL per provider.
func TestParseCloudFallback_valid(t *testing.T) {
	t.Setenv("ATLAS_TEST_ANTH_KEY", "sk-ant-123")
	targets, err := parseCloudFallback([]string{"qwen-7b:anthropic:claude-sonnet-4-6:ATLAS_TEST_ANTH_KEY"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, ok := targets["qwen-7b"]
	if !ok {
		t.Fatalf("no target for qwen-7b: %v", targets)
	}
	if got.Provider != server.CloudProviderAnthropic || got.Model != "claude-sonnet-4-6" || got.APIKey != "sk-ant-123" {
		t.Fatalf("target = %+v", got)
	}
	if got.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("base URL = %q, want the anthropic default", got.BaseURL)
	}
}

// TestParseCloudFallback_customBaseURL: a base URL with a scheme (colons) survives
// the colon-delimited spec.
func TestParseCloudFallback_customBaseURL(t *testing.T) {
	t.Setenv("ATLAS_TEST_OAI_KEY", "sk-oai")
	targets, err := parseCloudFallback([]string{"m:openai:gpt-4o:ATLAS_TEST_OAI_KEY:https://proxy.internal:8443"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := targets["m"].BaseURL; got != "https://proxy.internal:8443" {
		t.Fatalf("base URL = %q, want the custom URL intact", got)
	}
}

// TestParseCloudFallback_empty: no specs means fallback off (nil targets, no error).
func TestParseCloudFallback_empty(t *testing.T) {
	targets, err := parseCloudFallback(nil)
	if err != nil || targets != nil {
		t.Fatalf("empty = (%v, %v), want (nil, nil)", targets, err)
	}
}

// TestParseCloudFallback_errors: malformed specs are rejected with a clear error.
func TestParseCloudFallback_errors(t *testing.T) {
	t.Setenv("ATLAS_TEST_KEY", "sk")
	cases := map[string][]string{
		"too few fields":  {"m:anthropic:up"},
		"bad provider":    {"m:cohere:up:ATLAS_TEST_KEY"},
		"missing key env": {"m:anthropic:up:ATLAS_TEST_UNSET_KEY"},
		"duplicate model": {"m:anthropic:up:ATLAS_TEST_KEY", "m:openai:up2:ATLAS_TEST_KEY"},
	}
	for name, specs := range cases {
		if _, err := parseCloudFallback(specs); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}
