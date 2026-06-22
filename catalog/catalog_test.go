package catalog

import (
	"strings"
	"testing"
)

// TestStarterCatalogValid asserts the embedded starter catalog parses, every
// entry validates, and the small llama.cpp tier needed for the phase-9
// cold-boot gate is present and fully pinned.
func TestStarterCatalogValid(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.All()) == 0 {
		t.Fatal("starter catalog is empty")
	}

	var gguf int
	tiers := map[string]bool{}
	for _, e := range c.All() {
		tiers[e.Tier] = true
		if e.Source.Type == "gguf" {
			gguf++
			if e.Engine != "llamacpp" {
				t.Errorf("%s: gguf entry not on llamacpp", e.Name)
			}
		}
	}
	// At least one reasoning and one non-reasoning gguf model so the CPU
	// conformance gate (G4) can exercise both profiles from the catalog.
	if gguf < 2 {
		t.Errorf("want >=2 gguf (CPU-bootable) entries, got %d", gguf)
	}
	var haveReasoning, haveNonReasoning bool
	for _, e := range c.All() {
		if e.Source.Type != "gguf" {
			continue
		}
		if e.Reasoning {
			haveReasoning = true
		} else {
			haveNonReasoning = true
		}
	}
	if !haveReasoning || !haveNonReasoning {
		t.Errorf("gguf tier must cover both profiles: reasoning=%v non-reasoning=%v", haveReasoning, haveNonReasoning)
	}
}

func TestLookup(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := c.Lookup("qwen2.5-1.5b-instruct"); !ok {
		t.Error("expected qwen2.5-1.5b-instruct in catalog")
	}
	if _, ok := c.Lookup("not-a-model"); ok {
		t.Error("unexpected hit for unknown model")
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	cases := map[string]string{
		"bad name":     `models: [{name: "Bad Name", engine: llamacpp, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
		"bad engine":   `models: [{name: m, engine: sglang, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
		"bad tier":     `models: [{name: m, engine: llamacpp, tier: tiny, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
		"no ctx":       `models: [{name: m, engine: llamacpp, tier: haiku, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
		"gguf no sha":  `models: [{name: m, engine: llamacpp, tier: haiku, context_window: 1, source: {type: gguf, url: u}}]`,
		"gguf vllm":    `models: [{name: m, engine: vllm, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
		"hf no repo":   `models: [{name: m, engine: vllm, tier: haiku, context_window: 1, source: {type: hf}}]`,
		"short sha":    `models: [{name: m, engine: llamacpp, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: abc}}]`,
		"bad class":    `models: [{name: m, class: vision, engine: llamacpp, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
		"chat no tier": `models: [{name: m, engine: llamacpp, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`,
	}
	for name, doc := range cases {
		if _, err := parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

// TestEmbeddingClassValid: an embedding-class entry is valid without a tier (it is
// addressed by name, not via the chat aliases), and ClassOrChat reports its class
// while a tier-less chat default reads back as chat.
func TestEmbeddingClassValid(t *testing.T) {
	doc := `models: [{name: emb, class: embedding, engine: llamacpp, context_window: 512, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}]`
	c, err := parse([]byte(doc))
	if err != nil {
		t.Fatalf("embedding entry without a tier should be valid: %v", err)
	}
	e, ok := c.Lookup("emb")
	if !ok || e.ClassOrChat() != ClassEmbedding {
		t.Fatalf("ClassOrChat = %q (ok=%v), want embedding", e.ClassOrChat(), ok)
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	doc := `models:
  - {name: dup, engine: llamacpp, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("a", 64) + `}}
  - {name: dup, engine: llamacpp, tier: haiku, context_window: 1, source: {type: gguf, url: u, sha256: ` + strings.Repeat("b", 64) + `}}`
	if _, err := parse([]byte(doc)); err == nil {
		t.Error("expected duplicate-name error")
	}
}
