package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func runPullCapture(t *testing.T, args []string) (string, error) {
	t.Helper()
	cmd := testCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runPull(context.Background(), cmd, &pullOptions{stateDir: t.TempDir()}, args)
	return out.String(), err
}

func TestPullListsCatalog(t *testing.T) {
	out, err := runPullCapture(t, nil)
	if err != nil {
		t.Fatalf("runPull: %v", err)
	}
	if !strings.Contains(out, "qwen2.5-1.5b-instruct") || !strings.Contains(out, "NAME") {
		t.Errorf("listing missing header/entry:\n%s", out)
	}
}

func TestPullUnknownModel(t *testing.T) {
	if _, err := runPullCapture(t, []string{"no-such-model"}); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestPullHFIsNoop(t *testing.T) {
	// A vLLM/HF catalog entry is fetched by the engine at boot — pull just says so
	// and touches no network.
	out, err := runPullCapture(t, []string{"glm-5.1"})
	if err != nil {
		t.Fatalf("runPull: %v", err)
	}
	if !strings.Contains(out, "nothing to pre-pull") {
		t.Errorf("expected no-op message, got:\n%s", out)
	}
}
