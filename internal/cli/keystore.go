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
//
// The state dir is created (and re-tightened if it predates this) as owner-only
// 0700: it holds the control-plane DB — API-key hashes and the usage ledger —
// plus the self-signed TLS private key, so it must not be readable by other
// local users. 0700 on the directory protects the DB and its -wal/-shm
// side-files in one step regardless of their own modes.
func openStateDB(stateDir string) (*db.DB, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure state dir: %w", err)
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
	return u.db.RecordUsage(ctx, toDBUsage(rec))
}

// RecordBatch implements server.BatchUsageRecorder, so the async usage writer
// flushes a whole batch in one transaction (M2 phase 2b).
func (u usageRecorder) RecordBatch(ctx context.Context, recs []server.UsageRecord) error {
	rows := make([]db.UsageRecord, len(recs))
	for i, rec := range recs {
		rows[i] = toDBUsage(rec)
	}
	return u.db.RecordUsageBatch(ctx, rows)
}

func toDBUsage(rec server.UsageRecord) db.UsageRecord {
	return db.UsageRecord{
		KeyID:        rec.KeyID,
		Model:        rec.Model,
		WorkerID:     rec.WorkerID,
		InputTokens:  rec.InputTokens,
		OutputTokens: rec.OutputTokens,
	}
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
