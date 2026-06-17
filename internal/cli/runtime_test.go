package cli

import (
	"bytes"
	"strings"
	"testing"
)

// runProvision executes `atlas runtime provision` with the given args, capturing
// output. It never reaches the network for an invalid engine (validation fails
// first), which is what these tests exercise.
func runProvision(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRuntimeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"provision"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestRuntimeProvisionRejectsUnknownEngine(t *testing.T) {
	_, err := runProvision(t, "--engine", "tensorrt", "--state-dir", t.TempDir())
	if err == nil {
		t.Fatal("expected an error for an unknown engine")
	}
	if !strings.Contains(err.Error(), "invalid --engine") {
		t.Errorf("unexpected error: %v", err)
	}
}
