package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditEntry is one control-plane mutation recorded in the append-only audit log
// (M3 phase 3). Actor is who performed it — an admin API key id for an HTTP control
// action, or "cli"/"system" for local key management. Action is a stable label
// (e.g. "deployment.set", "worker.drain", "key.create"); Target is the resource it
// acted on (a model name, worker id, or key id); Result is "ok" or "error"; Detail
// holds optional context (an HTTP status, a replica count, an error message). Time
// is set by RecordAudit when zero.
type AuditEntry struct {
	ID     int64
	Time   time.Time
	Actor  string
	Action string
	Target string
	Result string
	Detail string
}

// AuditFilter narrows a ListAudit query. Empty string fields and zero times are
// ignored (no filter on that column); Limit caps the number of rows (<= 0 applies a
// default cap so an unbounded log cannot return an unbounded result).
type AuditFilter struct {
	Actor  string
	Action string
	Target string
	Since  time.Time
	Until  time.Time
	Limit  int
}

// defaultAuditLimit bounds a ListAudit result when the caller sets no limit, so the
// CLI and the read API never materialize an arbitrarily large log at once.
const defaultAuditLimit = 500

// RecordAudit appends one row to the audit log, stamped with the current UTC time
// when Entry.Time is zero. The log is append-only: this is the only writer, and
// there is deliberately no update or delete method. Callers off the request hot path
// (admin mutations, key management) may treat a failure as non-fatal and log it.
func (d *DB) RecordAudit(ctx context.Context, e AuditEntry) error {
	ts := e.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO audit (ts, actor, action, target, result, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		ts.UTC().Format(time.RFC3339), e.Actor, e.Action, e.Target, e.Result, e.Detail)
	if err != nil {
		return fmt.Errorf("db: record audit: %w", err)
	}
	return nil
}

// ListAudit returns audit rows matching the filter, newest first. It is read-only;
// the log is never mutated. Time bounds are inclusive on Since and exclusive on
// Until, comparing the stored RFC3339 strings (which sort lexically in time order).
func (d *DB) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	var (
		where []string
		args  []any
	)
	if f.Actor != "" {
		where = append(where, "actor = ?")
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.Target != "" {
		where = append(where, "target = ?")
		args = append(args, f.Target)
	}
	if !f.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.Since.UTC().Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, f.Until.UTC().Format(time.RFC3339))
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}

	query := "SELECT id, ts, actor, action, target, result, detail FROM audit"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Newest first: ts descending, then id descending to break ties within a second
	// (the stamp has 1s resolution) by insertion order.
	query += " ORDER BY ts DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []AuditEntry
	for rows.Next() {
		var (
			e  AuditEntry
			ts string
		)
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.Result, &e.Detail); err != nil {
			return nil, fmt.Errorf("db: list audit: scan: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
			e.Time = t
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list audit: %w", err)
	}
	return entries, nil
}
