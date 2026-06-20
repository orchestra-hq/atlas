package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenStateDBSecuresStateDir asserts the control-plane state dir — which
// holds API-key hashes, the usage ledger, and the self-signed TLS key — is
// owner-only (0700), both when freshly created and when it predates this as a
// group/world-readable dir.
func TestOpenStateDBSecuresStateDir(t *testing.T) {
	assertOwnerOnly := func(t *testing.T, dir string) {
		t.Helper()
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("state dir mode = %04o, want 0700 (no group/other access to key hashes + TLS key)", perm)
		}
	}

	t.Run("fresh dir is created owner-only", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		store, err := openStateDB(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		assertOwnerOnly(t, dir)
	})

	t.Run("pre-existing loose dir is re-tightened", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "loose")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		store, err := openStateDB(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		assertOwnerOnly(t, dir)
	})
}
