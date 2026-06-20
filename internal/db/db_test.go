package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// open returns a DB backed by a fresh temp-dir file, closed at test end.
func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "atlas.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestCreateAndAuthenticate(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	secret, k, err := d.CreateKey(ctx, []string{"model-a"}, true)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if secret == "" || k.ID == "" {
		t.Fatalf("empty secret/id: %q %q", secret, k.ID)
	}
	if k.Prefix != secret[:keyPrefixLen] {
		t.Errorf("prefix %q not a prefix of secret %q", k.Prefix, secret)
	}

	got, err := d.AuthenticateKey(ctx, secret)
	if err != nil {
		t.Fatalf("AuthenticateKey: %v", err)
	}
	if got.ID != k.ID || !got.Admin || len(got.Allowlist) != 1 || got.Allowlist[0] != "model-a" {
		t.Errorf("authenticated key = %+v", got)
	}
	if got.Revoked() {
		t.Error("fresh key reads as revoked")
	}
}

func TestAuthenticateUnknownSecret(t *testing.T) {
	d := open(t)
	if _, err := d.AuthenticateKey(context.Background(), "atlas-nope"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("AuthenticateKey(unknown) err = %v, want ErrKeyNotFound", err)
	}
}

func TestSecretIsNotRecoverable(t *testing.T) {
	// The store keeps only a hash: two keys never collide, and the clear-text
	// secret is returned only at creation.
	d := open(t)
	ctx := context.Background()
	s1, _, err := d.CreateKey(ctx, nil, false)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	s2, _, err := d.CreateKey(ctx, nil, false)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if s1 == s2 {
		t.Fatal("two created keys share a secret")
	}
	if hashSecret(s1) == hashSecret(s2) {
		t.Fatal("two created keys share a hash")
	}
}

func TestEmptyAllowlistMeansAll(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	secret, _, err := d.CreateKey(ctx, nil, false)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	got, err := d.AuthenticateKey(ctx, secret)
	if err != nil {
		t.Fatalf("AuthenticateKey: %v", err)
	}
	if len(got.Allowlist) != 0 {
		t.Errorf("allowlist = %v, want empty (all models)", got.Allowlist)
	}
	if got.Admin {
		t.Error("non-admin key reads as admin")
	}
}

func TestRevokeTakesEffect(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	secret, k, err := d.CreateKey(ctx, nil, false)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := d.AuthenticateKey(ctx, secret); err != nil {
		t.Fatalf("pre-revoke auth: %v", err)
	}
	if err := d.RevokeKey(ctx, k.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := d.AuthenticateKey(ctx, secret); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("post-revoke auth err = %v, want ErrKeyNotFound", err)
	}
	// Idempotent: revoking again still succeeds.
	if err := d.RevokeKey(ctx, k.ID); err != nil {
		t.Errorf("second RevokeKey: %v", err)
	}
}

func TestRevokeUnknownKey(t *testing.T) {
	d := open(t)
	if err := d.RevokeKey(context.Background(), "key_does_not_exist"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("RevokeKey(unknown) = %v, want ErrKeyNotFound", err)
	}
}

func TestListKeysShowsRevokedStatus(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	_, live, err := d.CreateKey(ctx, nil, true)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	_, dead, err := d.CreateKey(ctx, []string{"m"}, false)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := d.RevokeKey(ctx, dead.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	keys, err := d.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListKeys returned %d keys, want 2", len(keys))
	}
	byID := map[string]Key{}
	for _, k := range keys {
		byID[k.ID] = k
	}
	if byID[live.ID].Revoked() {
		t.Error("live key reads as revoked")
	}
	if !byID[dead.ID].Revoked() {
		t.Error("revoked key reads as live")
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atlas.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	secret, _, err := d1.CreateKey(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second open (e.g. the server, after `atlas keys create` wrote the file)
	// sees the key and re-running migrate is a no-op.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer func() { _ = d2.Close() }()
	if _, err := d2.AuthenticateKey(context.Background(), secret); err != nil {
		t.Errorf("authenticate after reopen: %v", err)
	}
}
