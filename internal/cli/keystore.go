package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/orchestra-hq/atlas/internal/db"
	"github.com/orchestra-hq/atlas/internal/server"
)

// dbFileName is the control plane's SQLite database, under the state dir.
const dbFileName = "atlas.db"

// openStateDB ensures the state dir exists and opens the control-plane database
// inside it. Both the long-running server and the short-lived `atlas keys`
// commands open the same file; SQLite (WAL + busy timeout) handles the
// concurrent access (ADR-0008).
func openStateDB(stateDir string) (*db.DB, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return db.Open(filepath.Join(stateDir, dbFileName))
}

// keyAuth adapts the SQLite key store to the gateway's server.Authenticator:
// an unknown or revoked secret is "not authenticated" (ok=false), any other
// store error is surfaced so the gateway answers 500 rather than 401.
type keyAuth struct{ db *db.DB }

func (a keyAuth) Authenticate(ctx context.Context, secret string) (server.Identity, bool, error) {
	k, err := a.db.AuthenticateKey(ctx, secret)
	if errors.Is(err, db.ErrKeyNotFound) {
		return server.Identity{}, false, nil
	}
	if err != nil {
		return server.Identity{}, false, err
	}
	return server.Identity{KeyID: k.ID, Allowlist: k.Allowlist, Admin: k.Admin}, true, nil
}

// usageRecorder adapts the SQLite ledger to the gateway's server.UsageRecorder,
// translating the server's consumer-defined record into the db row (phase 6).
type usageRecorder struct{ db *db.DB }

func (u usageRecorder) Record(ctx context.Context, rec server.UsageRecord) error {
	return u.db.RecordUsage(ctx, db.UsageRecord{
		KeyID:        rec.KeyID,
		Model:        rec.Model,
		WorkerID:     rec.WorkerID,
		InputTokens:  rec.InputTokens,
		OutputTokens: rec.OutputTokens,
	})
}

// bootstrapDefaultKey mints a default full-access admin key when the store has
// no keys yet, so a freshly-started node is usable without a manual
// `atlas keys create` (ADR-0008). It returns the new secret and created=true on
// first run; on later starts the store already has keys, so it returns
// created=false and the operator uses a key they saved (the secret cannot be
// re-derived from its hash).
func bootstrapDefaultKey(ctx context.Context, d *db.DB) (secret string, created bool, err error) {
	keys, err := d.ListKeys(ctx)
	if err != nil {
		return "", false, err
	}
	if len(keys) > 0 {
		return "", false, nil
	}
	secret, _, err = d.CreateKey(ctx, nil, true)
	if err != nil {
		return "", false, err
	}
	return secret, true, nil
}
