package modelmeta

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		caps       Capabilities
		wantFamily string
		wantOK     bool
		wantReason bool
	}{
		{
			name:       "qwen2.5 by model_type",
			caps:       Capabilities{ModelType: "qwen2", Architecture: "Qwen2ForCausalLM"},
			wantFamily: "qwen2", wantOK: true, wantReason: false,
		},
		{
			name:       "qwen3 by model_type",
			caps:       Capabilities{ModelType: "qwen3", Architecture: "Qwen3ForCausalLM"},
			wantFamily: "qwen3", wantOK: true, wantReason: true,
		},
		{
			name:       "glm by model_type",
			caps:       Capabilities{ModelType: "glm4_moe", Architecture: "Glm4MoeForCausalLM"},
			wantFamily: "glm", wantOK: true, wantReason: true,
		},
		{
			name:       "gguf arch mirrored into model_type",
			caps:       Capabilities{Format: FormatGGUF, ModelType: "qwen3", Architecture: "qwen3"},
			wantFamily: "qwen3", wantOK: true, wantReason: true,
		},
		{
			name:       "architecture fallback when model_type absent",
			caps:       Capabilities{Architecture: "LlamaForCausalLM"},
			wantFamily: "llama", wantOK: true, wantReason: false,
		},
		{
			name:   "unknown model_type is not a known family",
			caps:   Capabilities{ModelType: "mamba", Architecture: "MambaForCausalLM"},
			wantOK: false,
		},
		{
			name:   "empty metadata is not a known family",
			caps:   Capabilities{},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := Classify(tc.caps)
			if ok != tc.wantOK {
				t.Fatalf("Classify ok = %v, want %v (family %q)", ok, tc.wantOK, f.Name)
			}
			if !ok {
				return
			}
			if f.Name != tc.wantFamily {
				t.Errorf("family = %q, want %q", f.Name, tc.wantFamily)
			}
			if f.Reasoning != tc.wantReason {
				t.Errorf("reasoning = %v, want %v", f.Reasoning, tc.wantReason)
			}
		})
	}
}

func TestFamilyEngineArgs(t *testing.T) {
	qwen3, _ := Classify(Capabilities{ModelType: "qwen3"})
	qwen2, _ := Classify(Capabilities{ModelType: "qwen2"})
	gemma, _ := Classify(Capabilities{ModelType: "gemma2"})

	cases := []struct {
		name   string
		family Family
		engine string
		want   []string
	}{
		{"qwen3 vllm", qwen3, "vllm", []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes", "--reasoning-parser", "qwen3"}},
		{"qwen3 sglang", qwen3, "sglang", []string{"--tool-call-parser", "qwen25", "--reasoning-parser", "qwen3"}},
		{"qwen3 llamacpp (template-driven)", qwen3, "llamacpp", nil},
		{"qwen3 mlx (template-driven)", qwen3, "mlx", nil},
		{"qwen2 vllm omits reasoning parser", qwen2, "vllm", []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes"}},
		{"gemma vllm (no seeded parser)", gemma, "vllm", nil},
		{"gemma sglang (no seeded parser)", gemma, "sglang", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.family.EngineArgs(tc.engine)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("EngineArgs(%q) = %v, want %v", tc.engine, got, tc.want)
			}
		})
	}
}

// VerdictFor (and thus Inspect) reports a real family, not the Pending sentinel.
func TestVerdictReportsFamily(t *testing.T) {
	known := VerdictFor(Capabilities{ModelType: "qwen3", Engines: []string{"vllm"}})
	if known.Family != "qwen3" {
		t.Errorf("family = %q, want qwen3", known.Family)
	}
	unknown := VerdictFor(Capabilities{ModelType: "mamba"})
	if unknown.Family != FamilyUnknown {
		t.Errorf("family = %q, want %q", unknown.Family, FamilyUnknown)
	}
}
