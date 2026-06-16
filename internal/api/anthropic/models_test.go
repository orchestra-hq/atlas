package anthropic

import (
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

func TestNewModelInfoDefaultsDisplayName(t *testing.T) {
	m := NewModelInfo("real-a", "", "2026-06-16T00:00:00Z", 4096)
	if m.Type != "model" {
		t.Errorf("type = %q", m.Type)
	}
	if m.DisplayName != "real-a" {
		t.Errorf("display_name = %q, want id fallback", m.DisplayName)
	}
	if m.ContextWindow != 4096 {
		t.Errorf("context_window = %d", m.ContextWindow)
	}
}

func TestNewModelListSetsBounds(t *testing.T) {
	list := NewModelList([]ModelInfo{
		NewModelInfo("a", "", "t", 1),
		NewModelInfo("b", "", "t", 2),
	})
	if list.HasMore {
		t.Error("has_more should be false (no pagination in M0)")
	}
	if list.FirstID == nil || *list.FirstID != "a" || list.LastID == nil || *list.LastID != "b" {
		t.Errorf("bounds = %v / %v", list.FirstID, list.LastID)
	}

	empty := NewModelList(nil)
	if empty.FirstID != nil || empty.LastID != nil {
		t.Errorf("empty bounds = %v / %v", empty.FirstID, empty.LastID)
	}
}

func TestCountTokensToCore(t *testing.T) {
	req := CountTokensRequest{
		Model:    "m",
		System:   StringOrBlocks{Blocks: []WireBlock{{Type: "text", Text: "be terse"}}},
		Messages: []WireMessage{{Role: "user", Content: StringOrBlocks{Blocks: []WireBlock{{Type: "text", Text: "hi"}}}}},
		Thinking: &WireThinking{Type: "enabled"},
	}
	core, err := req.ToCore()
	if err != nil {
		t.Fatalf("ToCore: %v", err)
	}
	if core.MaxTokens != 0 {
		t.Errorf("max_tokens = %d, want 0 (count_tokens has none)", core.MaxTokens)
	}
	if core.System != "be terse" || len(core.Messages) != 1 {
		t.Errorf("translation lost fields: %+v", core)
	}
	if core.Thinking == nil || !core.Thinking.Enabled {
		t.Errorf("thinking = %v", core.Thinking)
	}
}

func TestCountTokensToCoreValidates(t *testing.T) {
	// Missing model reuses the shared Messages validation.
	if _, err := (&CountTokensRequest{Messages: []WireMessage{{Role: "user"}}}).ToCore(); err == nil {
		t.Error("expected error for missing model")
	}
}

// guard against an accidental import cycle in the test's use of core.
var _ = core.Request{}
