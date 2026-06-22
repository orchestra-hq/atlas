package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/db"
)

// runAudit runs `atlas audit` with args against stateDir, returning captured output.
func runAudit(t *testing.T, stateDir string, args ...string) (string, error) {
	t.Helper()
	cmd := newAuditCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--state-dir", stateDir))
	err := cmd.Execute()
	return out.String(), err
}

// seedAudit writes a few audit rows into the store under stateDir.
func seedAudit(t *testing.T, stateDir string) {
	t.Helper()
	store, err := openStateDB(stateDir)
	if err != nil {
		t.Fatalf("openStateDB: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	for _, e := range []db.AuditEntry{
		{Actor: "cli", Action: "key.create", Target: "key_abc", Result: "ok"},
		{Actor: "admin1", Action: "deployment.set", Target: "qwen-7b", Result: "ok"},
		{Actor: "admin1", Action: "worker.drain", Target: "w9", Result: "error"},
	} {
		if err := store.RecordAudit(ctx, e); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}
}

// TestAuditCmd_listsAndFilters: `atlas audit` lists entries and narrows by --action
// and --actor (G21).
func TestAuditCmd_listsAndFilters(t *testing.T) {
	dir := t.TempDir()
	seedAudit(t, dir)

	all, err := runAudit(t, dir)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, want := range []string{"key.create", "deployment.set", "worker.drain", "qwen-7b", "key_abc"} {
		if !strings.Contains(all, want) {
			t.Fatalf("listing missing %q:\n%s", want, all)
		}
	}

	byAction, err := runAudit(t, dir, "--action", "deployment.set")
	if err != nil {
		t.Fatalf("audit --action: %v", err)
	}
	if !strings.Contains(byAction, "qwen-7b") || strings.Contains(byAction, "worker.drain") {
		t.Fatalf("--action deployment.set did not narrow output:\n%s", byAction)
	}

	byActor, err := runAudit(t, dir, "--actor", "cli")
	if err != nil {
		t.Fatalf("audit --actor: %v", err)
	}
	if !strings.Contains(byActor, "key.create") || strings.Contains(byActor, "deployment.set") {
		t.Fatalf("--actor cli did not narrow output:\n%s", byActor)
	}
}

// TestAuditCmd_emptyLog: with nothing recorded, the command says so cleanly.
func TestAuditCmd_emptyLog(t *testing.T) {
	out, err := runAudit(t, t.TempDir())
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !strings.Contains(out, "No audit entries") {
		t.Fatalf("empty log output = %q, want the no-entries message", out)
	}
}

// TestAuditCmd_json: --json emits the rows as a JSON array.
func TestAuditCmd_json(t *testing.T) {
	dir := t.TempDir()
	seedAudit(t, dir)
	out, err := runAudit(t, dir, "--json", "--action", "key.create")
	if err != nil {
		t.Fatalf("audit --json: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "[") || !strings.Contains(out, `"action": "key.create"`) {
		t.Fatalf("--json output not a JSON array of rows:\n%s", out)
	}
}
