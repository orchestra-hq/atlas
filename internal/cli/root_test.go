package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/version"
)

func TestVersionCommand(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := out.String(), "atlas "+version.String()+"\n"; got != want {
		t.Errorf("version output = %q, want %q", got, want)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"definitely-not-a-command"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "definitely-not-a-command") {
		t.Errorf("Execute() error = %v, want unknown-command error", err)
	}
}
