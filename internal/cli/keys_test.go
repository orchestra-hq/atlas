package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// runKeys runs the `keys` subcommand tree with args against stateDir, returning
// captured stdout. The --state-dir flag is appended so every subcommand hits
// the same temp store.
func runKeys(t *testing.T, stateDir string, args ...string) (string, error) {
	t.Helper()
	cmd := newKeysCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--state-dir", stateDir))
	err := cmd.Execute()
	return out.String(), err
}

func TestKeysCreateListRevoke(t *testing.T) {
	dir := t.TempDir()

	// create
	out, err := runKeys(t, dir, "create", "--allow", "model-a", "--admin")
	if err != nil {
		t.Fatalf("keys create: %v", err)
	}
	if !strings.Contains(out, "Secret") || !strings.Contains(out, "atlas-") {
		t.Fatalf("create output missing secret:\n%s", out)
	}
	if !strings.Contains(out, "admin") || !strings.Contains(out, "model-a") {
		t.Errorf("create output missing scope/allowlist:\n%s", out)
	}

	// list shows the key, its scope, allowlist, active status — never the secret.
	out, err = runKeys(t, dir, "list")
	if err != nil {
		t.Fatalf("keys list: %v", err)
	}
	for _, want := range []string{"KEY ID", "key_", "admin", "model-a", "active"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	// Pull the id back out of the store to revoke it.
	store, err := openStateDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := store.ListKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys: %v (n=%d)", err, len(keys))
	}
	id := keys[0].ID
	_ = store.Close()

	out, err = runKeys(t, dir, "revoke", id)
	if err != nil {
		t.Fatalf("keys revoke: %v", err)
	}
	if !strings.Contains(out, "Revoked") {
		t.Errorf("revoke output:\n%s", out)
	}

	out, err = runKeys(t, dir, "list")
	if err != nil {
		t.Fatalf("keys list after revoke: %v", err)
	}
	if !strings.Contains(out, "revoked") {
		t.Errorf("list after revoke missing revoked status:\n%s", out)
	}
}

func TestKeysCreateQuiet(t *testing.T) {
	dir := t.TempDir()
	out, err := runKeys(t, dir, "create", "--quiet")
	if err != nil {
		t.Fatalf("keys create --quiet: %v", err)
	}
	secret := strings.TrimSpace(out)
	// Quiet output is exactly the secret — one line, no labels.
	if !strings.HasPrefix(secret, "atlas-") || strings.ContainsAny(secret, " \t") {
		t.Fatalf("quiet output is not a bare secret: %q", out)
	}
	// And it authenticates through the adapter.
	store, err := openStateDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, ok, err := (keyAuth{db: store}).Authenticate(context.Background(), secret); !ok || err != nil {
		t.Errorf("quiet secret does not authenticate: ok=%v err=%v", ok, err)
	}
}

func TestKeysRevokeUnknown(t *testing.T) {
	dir := t.TempDir()
	if _, err := runKeys(t, dir, "revoke", "key_nope"); err == nil {
		t.Fatal("revoke of unknown key should error")
	}
}

func TestBootstrapDefaultKey(t *testing.T) {
	store, err := openStateDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	// First run mints and returns a key.
	secret, created, err := bootstrapDefaultKey(ctx, store)
	if err != nil || !created || secret == "" {
		t.Fatalf("first bootstrap: secret=%q created=%v err=%v", secret, created, err)
	}
	// The minted key is a full-access admin key, usable through the gateway adapter.
	id, ok, err := (keyAuth{db: store}).Authenticate(ctx, secret)
	if err != nil || !ok {
		t.Fatalf("authenticate default key: ok=%v err=%v", ok, err)
	}
	if !id.Admin || len(id.Allowlist) != 0 {
		t.Errorf("default key identity = %+v, want admin + no allowlist", id)
	}

	// Second run is a no-op: keys already exist.
	s2, created2, err := bootstrapDefaultKey(ctx, store)
	if err != nil || created2 || s2 != "" {
		t.Errorf("second bootstrap: secret=%q created=%v err=%v", s2, created2, err)
	}
}

func TestKeyAuthRejectsUnknownAndRevoked(t *testing.T) {
	store, err := openStateDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	auth := keyAuth{db: store}

	// Unknown secret: not authenticated, no error.
	if _, ok, err := auth.Authenticate(ctx, "atlas-nope"); ok || err != nil {
		t.Errorf("unknown secret: ok=%v err=%v", ok, err)
	}

	secret, k, err := store.CreateKey(ctx, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := auth.Authenticate(ctx, secret); !ok {
		t.Fatal("fresh key should authenticate")
	}
	if err := store.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := auth.Authenticate(ctx, secret); ok || err != nil {
		t.Errorf("revoked secret: ok=%v err=%v (want false, nil)", ok, err)
	}
}
