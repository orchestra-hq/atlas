package db

import (
	"context"
	"fmt"
	"time"
)

// UsageRecord is one completed inference request's token accounting, written to
// the ledger as the request finishes (phase 6). KeyID is the API key that made
// the call, Model the served (canonical) model name, WorkerID the stable
// identity of the worker that ran it — its operator-supplied --name ("local" for
// the in-process worker), so per-worker totals survive reconnects rather than
// fragmenting across ephemeral connection ids (M2 phase 1). Token counts are the engine's for
// a clean completion; for a stream cut off partway, OutputTokens is an estimate
// of what was emitted (the gateway's running count), so the ledger is not
// systematically short on interrupted requests.
type UsageRecord struct {
	KeyID        string
	Model        string
	WorkerID     string
	InputTokens  int
	OutputTokens int
}

// RecordUsage appends one row to the usage ledger, stamped with the current UTC
// time. It is on the request hot path, so callers treat a failure as
// non-fatal (the response is already served); the error is returned for logging.
func (d *DB) RecordUsage(ctx context.Context, u UsageRecord) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO usage (ts, key_id, model, worker_id, input_tokens, output_tokens) VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), u.KeyID, u.Model, u.WorkerID, u.InputTokens, u.OutputTokens)
	if err != nil {
		return fmt.Errorf("db: record usage: %w", err)
	}
	return nil
}

// RecordUsageBatch appends many rows in a single transaction — the bulk path the
// async usage writer flushes to, so per-request inserts stay off the request hot
// path (M2 phase 2b). All rows share one wall-clock stamp (the flush time); the
// per-request ordering they were enqueued in is not otherwise preserved, which the
// ledger's aggregate summaries do not depend on. An empty batch is a no-op.
func (d *DB) RecordUsageBatch(ctx context.Context, us []UsageRecord) error {
	if len(us) == 0 {
		return nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: record usage batch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO usage (ts, key_id, model, worker_id, input_tokens, output_tokens) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("db: record usage batch: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	ts := time.Now().UTC().Format(time.RFC3339)
	for _, u := range us {
		if _, err := stmt.ExecContext(ctx, ts, u.KeyID, u.Model, u.WorkerID, u.InputTokens, u.OutputTokens); err != nil {
			return fmt.Errorf("db: record usage batch: exec: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: record usage batch: commit: %w", err)
	}
	return nil
}

// UsageTotal is an aggregated slice of the ledger: all rows sharing one group
// value (a key id, a model, or a worker id, depending on the query), with their
// request count and summed token usage. Group is the shared value.
type UsageTotal struct {
	Group        string
	Requests     int
	InputTokens  int
	OutputTokens int
}

// usageGroupedBy sums the ledger grouped by a single column, ordered by the
// largest total token spend (ties broken by group name), so the busiest group
// leads the table. column is a trusted constant from the exported wrappers, never
// user input.
func (d *DB) usageGroupedBy(ctx context.Context, column string) ([]UsageTotal, error) {
	q := fmt.Sprintf(`
SELECT %[1]s AS grp, COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
FROM usage
GROUP BY %[1]s
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC, grp ASC`, column)
	rows, err := d.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: usage by %s: %w", column, err)
	}
	defer func() { _ = rows.Close() }()
	var totals []UsageTotal
	for rows.Next() {
		var t UsageTotal
		if err := rows.Scan(&t.Group, &t.Requests, &t.InputTokens, &t.OutputTokens); err != nil {
			return nil, fmt.Errorf("db: usage by %s: %w", column, err)
		}
		totals = append(totals, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: usage by %s: %w", column, err)
	}
	return totals, nil
}

// UsageByKey returns per-API-key usage totals.
func (d *DB) UsageByKey(ctx context.Context) ([]UsageTotal, error) {
	return d.usageGroupedBy(ctx, "key_id")
}

// UsageByModel returns per-model usage totals.
func (d *DB) UsageByModel(ctx context.Context) ([]UsageTotal, error) {
	return d.usageGroupedBy(ctx, "model")
}

// UsageByWorker returns per-worker usage totals.
func (d *DB) UsageByWorker(ctx context.Context) ([]UsageTotal, error) {
	return d.usageGroupedBy(ctx, "worker_id")
}
