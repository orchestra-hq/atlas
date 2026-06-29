package modelmeta

import (
	"strings"
	"testing"
)

func TestQuantToken(t *testing.T) {
	cases := map[string]string{
		"Qwen3-8B-Q4_K_M.gguf":              "Q4_K_M",
		"qwen2.5-1.5b-instruct-q4_k_m.gguf": "Q4_K_M",
		"Qwen3-0.6B-Q8_0.gguf":              "Q8_0",
		"model-IQ3_XXS.gguf":                "IQ3_XXS",
		"model-f16.gguf":                    "F16",
		"README.md":                         "",
	}
	for file, want := range cases {
		if got := QuantToken(file); got != want {
			t.Errorf("QuantToken(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestResolveQuantToken(t *testing.T) {
	caps := Capabilities{Files: []string{"m-Q4_K_M.gguf", "m-Q5_K_M.gguf", "m-Q8_0.gguf"}}

	// Exact and case-insensitive requests resolve to the canonical token.
	for _, want := range []string{"Q5_K_M", "q5_k_m"} {
		got, err := caps.ResolveQuantToken(want)
		if err != nil || got != "Q5_K_M" {
			t.Errorf("ResolveQuantToken(%q) = %q, %v; want Q5_K_M", want, got, err)
		}
	}

	// A request matching several quants is ambiguous.
	if _, err := caps.ResolveQuantToken("K_M"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ResolveQuantToken(K_M) err = %v, want ambiguous", err)
	}

	// A request matching nothing lists the available tokens.
	_, err := caps.ResolveQuantToken("Q3_K_S")
	if err == nil {
		t.Fatal("expected an error for an absent quant")
	}
	for _, want := range []string{"Q4_K_M", "Q5_K_M", "Q8_0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("absent-quant error %q missing %q", err, want)
		}
	}
}

func TestDefaultQuantToken(t *testing.T) {
	if got := (Capabilities{Selected: "m-Q4_K_M.gguf"}).DefaultQuantToken(); got != "Q4_K_M" {
		t.Errorf("DefaultQuantToken = %q, want Q4_K_M", got)
	}
	if got := (Capabilities{}).DefaultQuantToken(); got != "" {
		t.Errorf("DefaultQuantToken with no selection = %q, want empty", got)
	}
}
