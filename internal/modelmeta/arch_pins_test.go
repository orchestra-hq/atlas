package modelmeta

import (
	"testing"

	"github.com/orchestra-hq/atlas/internal/runtime"
)

// The pinned-version strings surfaced in the not-loadable message mirror
// internal/runtime's pins by hand (modelmeta is a parse leaf and does not import
// runtime in non-test code). This guards the mirror: a runtime pin bump that
// forgets arch.go would otherwise ship a refusal message citing a stale version.
func TestArchPinsTrackRuntime(t *testing.T) {
	cases := []struct {
		name, mirror, source string
	}{
		{"vllm", vllmPinned, runtime.VLLMVersion},
		{"sglang", sglangPinned, runtime.SGLangVersion},
		{"llamacpp", llamaCppPinned, runtime.LlamaCppTag},
		{"mlx", mlxPinned, runtime.MLXVersion},
	}
	for _, tc := range cases {
		if tc.mirror != tc.source {
			t.Errorf("%s pin mirror = %q, want %q (sync arch.go with internal/runtime)", tc.name, tc.mirror, tc.source)
		}
	}
}
