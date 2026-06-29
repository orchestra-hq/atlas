package modelmeta

import "testing"

func TestArchLoadable(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		caps   Capabilities
		want   bool
	}{
		{"qwen2 on vllm", "vllm", Capabilities{Architecture: "Qwen2ForCausalLM", ModelType: "qwen2"}, true},
		{"qwen3 on vllm", "vllm", Capabilities{Architecture: "Qwen3ForCausalLM", ModelType: "qwen3"}, true},
		{"qwen3 on sglang", "sglang", Capabilities{Architecture: "Qwen3ForCausalLM", ModelType: "qwen3"}, true},
		{"glm on vllm", "vllm", Capabilities{Architecture: "Glm4MoeForCausalLM", ModelType: "glm4_moe"}, true},
		{"llama on vllm", "vllm", Capabilities{Architecture: "LlamaForCausalLM", ModelType: "llama"}, true},
		{"gguf qwen3 on llamacpp", "llamacpp", Capabilities{Format: FormatGGUF, Architecture: "qwen3", ModelType: "qwen3"}, true},
		{"gguf llama on llamacpp", "llamacpp", Capabilities{Format: FormatGGUF, ModelType: "llama"}, true},
		{"mlx qwen3 by model_type", "mlx", Capabilities{Architecture: "Qwen3ForCausalLM", ModelType: "qwen3"}, true},
		// Broadened seed (P8.3 follow-up): DBRX, a text-capable VLM, and a GGUF token.
		{"dbrx on vllm", "vllm", Capabilities{Architecture: "DbrxForCausalLM", ModelType: "dbrx"}, true},
		{"qwen2.5-vl text on vllm", "vllm", Capabilities{Architecture: "Qwen2_5_VLForConditionalGeneration", ModelType: "qwen2_5_vl"}, true},
		{"gguf dbrx on llamacpp", "llamacpp", Capabilities{Format: FormatGGUF, ModelType: "dbrx"}, true},

		// Refusals — an architecture we have a string for but is not in the engine's set.
		{"unknown arch on vllm", "vllm", Capabilities{Architecture: "FooBarForCausalLM", ModelType: "foobar"}, false},
		{"unknown gguf arch on llamacpp", "llamacpp", Capabilities{Format: FormatGGUF, ModelType: "exoticnet"}, false},
		{"unknown type on mlx", "mlx", Capabilities{Architecture: "FooBarForCausalLM", ModelType: "foobar"}, false},

		// No architecture string to judge, or an unrecognized engine → don't block.
		{"no arch string on vllm", "vllm", Capabilities{}, true},
		{"unrecognized engine", "tgi", Capabilities{Architecture: "FooBarForCausalLM"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := ArchLoadable(tc.engine, tc.caps)
			if ok != tc.want {
				t.Fatalf("ArchLoadable(%q) = %v, want %v (reason %q)", tc.engine, ok, tc.want, reason)
			}
			if !ok && reason == "" {
				t.Error("refusal returned an empty reason")
			}
			if ok && reason != "" {
				t.Errorf("loadable returned a non-empty reason %q", reason)
			}
		})
	}
}

func TestFitEstimate(t *testing.T) {
	got, ok := FitEstimate(Capabilities{WeightBytes: 1000})
	if !ok {
		t.Fatal("FitEstimate ok = false for a known size")
	}
	if want := int64(1200); got != want { // 1000 * (1 + 0.2)
		t.Errorf("estimate = %d, want %d", got, want)
	}
	if _, ok := FitEstimate(Capabilities{WeightBytes: 0}); ok {
		t.Error("FitEstimate ok = true for an unknown size")
	}
}

func TestKVOverheadFractionShared(t *testing.T) {
	// The padding is the single shared constant the scheduler also references; guard
	// the value so a drift is a deliberate, reviewed change.
	if KVOverheadFraction != 0.2 {
		t.Errorf("KVOverheadFraction = %v, want 0.2", KVOverheadFraction)
	}
}
