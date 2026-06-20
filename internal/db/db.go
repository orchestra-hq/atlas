// Package db is the control plane's durable state: a single SQLite database,
// accessed through database/sql with the pure-Go modernc.org/sqlite driver so
// the single-binary cross-compile (GoReleaser, no cgo) stays intact. It opens
// at <state-dir>/atlas.db and is the one place control-plane state is persisted
// — API keys today (ADR-0008), the usage ledger next (phase 6). Going through
// database/sql keeps the Postgres door open for an eventual HA control plane
// without touching callers.
package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// ErrKeyNotFound is returned when a key id (revoke) or secret (authenticate)
// names no live key.
var ErrKeyNotFound = errors.New("db: key not found")

// DB is the control plane's SQLite store. Construct with Open; the zero value is
// not usable. It is safe for concurrent use (database/sql pools connections);
// writes are serialized by SQLite, reads run under WAL.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if absent) the SQLite database at path and applies the
// schema. WAL mode plus a busy timeout let the server read while a separate
// `atlas keys` process writes the same file, so key changes — including
// revocation — are visible across processes immediately (no cache; ADR-0008).
func Open(path string) (*DB, error) {
	// modernc.org/sqlite takes pragmas as DSN query params. WAL for concurrent
	// readers + one writer; busy_timeout so a cross-process write waits rather
	// than failing with SQLITE_BUSY; foreign_keys on for future tables.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open %q: %w", path, err)
	}
	d := &DB{sql: sqlDB}
	if err := d.migrate(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }

// migrations is the ordered list of schema steps; its length is the current
// schema version. Step i (1-based) is applied when PRAGMA user_version is below
// i, so a store created at an older version catches up step by step and a fresh
// store runs them all in order. Each step is idempotent (CREATE ... IF NOT
// EXISTS) as a belt-and-braces guard, but user_version is the real gate. To
// evolve the schema, append a new step; never edit or reorder an applied one.
var migrations = []string{
	// v1: the API key table. hash is the sha256 of the secret (high-entropy
	// machine tokens, not passwords — a fast indexed hash, not bcrypt; ADR-0008),
	// unique so a lookup by presented secret is an index hit. allowlist is a JSON
	// array of model names, empty = all models.
	`
CREATE TABLE IF NOT EXISTS api_keys (
	id          TEXT PRIMARY KEY,
	prefix      TEXT NOT NULL,
	hash        TEXT NOT NULL UNIQUE,
	allowlist   TEXT NOT NULL DEFAULT '[]',
	admin       INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	revoked_at  TEXT
);`,
	// v2: the usage ledger (phase 6). One row per completed inference request,
	// tagged with the calling key, the served model, and the worker that ran it,
	// so totals are queryable by any of the three. ts is RFC3339 UTC. Indexed on
	// the three group-by columns the `atlas usage` summaries scan.
	`
CREATE TABLE IF NOT EXISTS usage (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	ts            TEXT NOT NULL,
	key_id        TEXT NOT NULL,
	model         TEXT NOT NULL,
	worker_id     TEXT NOT NULL,
	input_tokens  INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_key_id ON usage(key_id);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage(model);
CREATE INDEX IF NOT EXISTS idx_usage_worker_id ON usage(worker_id);`,
}

// migrate brings the schema up to date by applying each migration step whose
// version exceeds the store's current PRAGMA user_version, in order. It is safe
// to run on every open: an up-to-date store applies nothing.
func (d *DB) migrate(ctx context.Context) error {
	var version int
	if err := d.sql.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("db: read schema version: %w", err)
	}
	for i, stmt := range migrations {
		target := i + 1
		if version >= target {
			continue
		}
		if _, err := d.sql.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("db: migrate to v%d: %w", target, err)
		}
		// PRAGMA user_version does not accept a bound parameter.
		if _, err := d.sql.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
			return fmt.Errorf("db: set schema version v%d: %w", target, err)
		}
		version = target
	}
	return nil
}

// Key is one API key record. The secret itself is shown only once, at creation;
// the store keeps only its sha256 hash, so a key cannot be recovered, only
// revoked and replaced.
type Key struct {
	ID        string     // stable identifier, e.g. "key_ab12cd34ef56"; used by list/revoke
	Prefix    string     // leading chars of the secret, for display only
	Allowlist []string   // model names this key may use; empty = all models
	Admin     bool       // carries the admin scope the /admin/* surface requires
	CreatedAt time.Time  // when the key was minted
	RevokedAt *time.Time // when it was revoked, nil if live
}

// Revoked reports whether the key has been revoked.
func (k Key) Revoked() bool { return k.RevokedAt != nil }

// CreateKey mints a new API key, returning the secret in clear text exactly
// once (only its hash is stored). allowlist is the model names the key may use
// (nil/empty = all); admin grants the scope the /admin/* surface requires.
func (d *DB) CreateKey(ctx context.Context, allowlist []string, admin bool) (secret string, k Key, err error) {
	secret = newSecret()
	if allowlist == nil {
		allowlist = []string{}
	}
	allowJSON, err := json.Marshal(allowlist)
	if err != nil {
		return "", Key{}, fmt.Errorf("db: encode allowlist: %w", err)
	}
	k = Key{
		ID:        newKeyID(),
		Prefix:    secret[:keyPrefixLen],
		Allowlist: allowlist,
		Admin:     admin,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	_, err = d.sql.ExecContext(ctx,
		`INSERT INTO api_keys (id, prefix, hash, allowlist, admin, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		k.ID, k.Prefix, hashSecret(secret), string(allowJSON), boolToInt(admin), k.CreatedAt.Format(time.RFC3339))
	if err != nil {
		return "", Key{}, fmt.Errorf("db: insert key: %w", err)
	}
	return secret, k, nil
}

// ListKeys returns every key, live and revoked, newest first.
func (d *DB) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, prefix, allowlist, admin, created_at, revoked_at FROM api_keys ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("db: list keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list keys: %w", err)
	}
	return keys, nil
}

// RevokeKey marks a key revoked by id. It is idempotent on a live key and
// returns ErrKeyNotFound if no key has that id; revoking an already-revoked key
// leaves its original revoked_at untouched and succeeds.
func (d *DB) RevokeKey(ctx context.Context, id string) error {
	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	res, err := d.sql.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id)
	if err != nil {
		return fmt.Errorf("db: revoke key: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// No live key updated: either unknown id, or already revoked. Distinguish so
	// the CLI can report "no such key" rather than silently succeeding.
	var exists int
	if err := d.sql.QueryRowContext(ctx, `SELECT 1 FROM api_keys WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrKeyNotFound
		}
		return fmt.Errorf("db: revoke key: %w", err)
	}
	return nil // already revoked
}

// AuthenticateKey resolves a presented secret to its live key, hashing the
// secret and looking it up by its indexed hash. A revoked or unknown secret
// returns ErrKeyNotFound. Called on every client request, so revocation takes
// effect immediately.
func (d *DB) AuthenticateKey(ctx context.Context, secret string) (Key, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT id, prefix, allowlist, admin, created_at, revoked_at FROM api_keys WHERE hash = ? AND revoked_at IS NULL`,
		hashSecret(secret))
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrKeyNotFound
	}
	if err != nil {
		return Key{}, err
	}
	return k, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanKey reads one api_keys row in the column order the SELECTs use.
func scanKey(row rowScanner) (Key, error) {
	var (
		k         Key
		allowJSON string
		admin     int
		createdAt string
		revokedAt sql.NullString
	)
	if err := row.Scan(&k.ID, &k.Prefix, &allowJSON, &admin, &createdAt, &revokedAt); err != nil {
		return Key{}, err
	}
	if err := json.Unmarshal([]byte(allowJSON), &k.Allowlist); err != nil {
		return Key{}, fmt.Errorf("db: decode allowlist for %s: %w", k.ID, err)
	}
	k.Admin = admin != 0
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return Key{}, fmt.Errorf("db: parse created_at for %s: %w", k.ID, err)
	}
	k.CreatedAt = t
	if revokedAt.Valid {
		r, err := time.Parse(time.RFC3339, revokedAt.String)
		if err != nil {
			return Key{}, fmt.Errorf("db: parse revoked_at for %s: %w", k.ID, err)
		}
		k.RevokedAt = &r
	}
	return k, nil
}

// keyPrefixLen is how many leading characters of a secret are stored for
// display ("atlas-" plus 8 hex chars), enough to tell keys apart at a glance
// without revealing the secret.
const keyPrefixLen = 14

// newSecret returns a fresh high-entropy API key secret. The "atlas-" prefix
// makes it recognizable in logs/configs; 24 random bytes (192 bits) is well
// beyond brute-force, which is why a single sha256 (not bcrypt) is sufficient.
func newSecret() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return "atlas-" + hex.EncodeToString(b[:])
}

// newKeyID returns a short stable identifier for a key, used by list/revoke.
func newKeyID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "key_" + hex.EncodeToString(b[:])
}

// hashSecret returns the hex sha256 of a secret — the value stored and matched.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
