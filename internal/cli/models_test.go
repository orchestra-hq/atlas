package cli

import (
	"reflect"
	"testing"
)

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
