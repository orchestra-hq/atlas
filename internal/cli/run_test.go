package cli

import (
	"strings"
	"testing"
)

func TestResolvePromptFromArgs(t *testing.T) {
	got, err := resolvePrompt(testCmd(), []string{"hello", "there"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello there" {
		t.Errorf("prompt = %q, want %q", got, "hello there")
	}
}

func TestResolvePromptFromStdin(t *testing.T) {
	cmd := testCmd()
	cmd.SetIn(strings.NewReader("piped prompt\n"))
	got, err := resolvePrompt(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "piped prompt" {
		t.Errorf("prompt = %q", got)
	}
}
