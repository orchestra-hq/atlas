package cli

import (
	"reflect"
	"testing"
)

// reserveFit weighs a model against the runtime's *free* capacity (total minus
// already-committed), reserving on success — the fleet/multi-model fix (M8 P8.3).
func TestEngineRuntimeReserveFit(t *testing.T) {
	const giB = 1 << 30
	rt := &engineRuntime{capacity: 10 * giB, hasGPU: true}

	if free, ok := rt.reserveFit(4 * giB); !ok || free != 10*giB {
		t.Fatalf("first reserve: free=%d ok=%v, want 10 GiB free, ok", free, ok)
	}
	if free, ok := rt.reserveFit(4 * giB); !ok || free != 6*giB {
		t.Fatalf("second reserve: free=%d ok=%v, want 6 GiB free, ok", free, ok)
	}
	// 2 GiB free now; a 4 GiB model does not fit and is refused (committed unchanged).
	if free, ok := rt.reserveFit(4 * giB); ok || free != 2*giB {
		t.Fatalf("over-commit: free=%d ok=%v, want 2 GiB free, refused", free, ok)
	}
	// Unloading one model frees its reservation, so the 4 GiB model now fits.
	rt.releaseFit(4 * giB)
	if _, ok := rt.reserveFit(4 * giB); !ok {
		t.Error("after release, a 4 GiB model should fit again")
	}
	// An unknown size always fits and reserves nothing.
	rt.committed = rt.capacity // full
	if _, ok := rt.reserveFit(0); !ok {
		t.Error("unknown size (0) must always fit")
	}
	// Unknown host capacity never refuses.
	none := &engineRuntime{capacity: 0}
	if _, ok := none.reserveFit(100 * giB); !ok {
		t.Error("unknown capacity (0) must not refuse")
	}
}

// mergeEngineArgs puts the user's --engine-arg after the model's defaults so that
// for argparse-style engines (last value wins) an explicit user flag overrides an
// auto-configured / catalog default of the same name.
func TestMergeEngineArgs(t *testing.T) {
	defaults := []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes"}
	user := []string{"--tool-call-parser", "mine"}

	got := mergeEngineArgs(defaults, user)
	want := []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes", "--tool-call-parser", "mine"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
	// The user's value is last, so an argparse engine resolves it as the winner.
	if li := lastValue(got, "--tool-call-parser"); li != "mine" {
		t.Errorf("last --tool-call-parser = %q, want the user's %q", li, "mine")
	}

	// Defaults are not mutated by the merge.
	if !reflect.DeepEqual(defaults, []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes"}) {
		t.Errorf("defaults mutated: %v", defaults)
	}
}

// lastValue returns the value following the last occurrence of flag, "" if absent.
func lastValue(args []string, flag string) string {
	last := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			last = args[i+1]
		}
	}
	return last
}
