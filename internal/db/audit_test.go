package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestAudit_recordAndListNewestFirst: recorded entries come back newest-first with
// their fields intact.
func TestAudit_recordAndListNewestFirst(t *testing.T) {
	d := open(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	for i, a := range []string{"key.create", "deployment.set", "worker.drain"} {
		if err := d.RecordAudit(ctx, AuditEntry{Time: base.Add(time.Duration(i) * time.Minute), Actor: "admin1", Action: a, Target: "t" + a, Result: "ok"}); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}

	got, err := d.ListAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Action != "worker.drain" || got[2].Action != "key.create" {
		t.Fatalf("order = [%s ... %s], want newest-first worker.drain ... key.create", got[0].Action, got[2].Action)
	}
	if got[0].Actor != "admin1" || got[0].Target != "tworker.drain" || got[0].Result != "ok" {
		t.Fatalf("fields not preserved: %+v", got[0])
	}
}

// TestAudit_filters: actor, action, target, time-window, and limit each narrow the
// result.
func TestAudit_filters(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)

	entries := []AuditEntry{
		{Time: base, Actor: "admin1", Action: "deployment.set", Target: "m1", Result: "ok"},
		{Time: base.Add(time.Minute), Actor: "cli", Action: "key.create", Target: "k1", Result: "ok"},
		{Time: base.Add(2 * time.Minute), Actor: "admin1", Action: "deployment.stop", Target: "m1", Result: "ok"},
		{Time: base.Add(3 * time.Minute), Actor: "admin2", Action: "worker.drain", Target: "w9", Result: "error"},
	}
	for _, e := range entries {
		if err := d.RecordAudit(ctx, e); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}

	if got, _ := d.ListAudit(ctx, AuditFilter{Actor: "admin1"}); len(got) != 2 {
		t.Fatalf("actor filter: got %d, want 2", len(got))
	}
	if got, _ := d.ListAudit(ctx, AuditFilter{Action: "key.create"}); len(got) != 1 || got[0].Target != "k1" {
		t.Fatalf("action filter: got %+v, want one key.create on k1", got)
	}
	if got, _ := d.ListAudit(ctx, AuditFilter{Target: "m1"}); len(got) != 2 {
		t.Fatalf("target filter: got %d, want 2", len(got))
	}
	// Window [base+1m, base+3m): excludes the first (==base) and last (==base+3m).
	if got, _ := d.ListAudit(ctx, AuditFilter{Since: base.Add(time.Minute), Until: base.Add(3 * time.Minute)}); len(got) != 2 {
		t.Fatalf("time window: got %d, want 2", len(got))
	}
	if got, _ := d.ListAudit(ctx, AuditFilter{Limit: 1}); len(got) != 1 {
		t.Fatalf("limit: got %d, want 1", len(got))
	}
}

// TestAudit_durableAcrossReopen: the log survives closing and reopening the store
// (G21 durability).
func TestAudit_durableAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "atlas.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d1.RecordAudit(ctx, AuditEntry{Actor: "admin1", Action: "deployment.set", Target: "m1", Result: "ok"}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}
	_ = d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = d2.Close() }()
	got, err := d2.ListAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit after reopen: %v", err)
	}
	if len(got) != 1 || got[0].Action != "deployment.set" {
		t.Fatalf("after reopen got %+v, want the one persisted entry", got)
	}
}

// TestAudit_defaultLimitCaps: with no limit set, the result is capped so an
// unbounded log cannot return an unbounded slice.
func TestAudit_defaultLimitCaps(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	for i := 0; i < defaultAuditLimit+10; i++ {
		if err := d.RecordAudit(ctx, AuditEntry{Actor: "a", Action: "x", Result: "ok"}); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}
	got, _ := d.ListAudit(ctx, AuditFilter{})
	if len(got) != defaultAuditLimit {
		t.Fatalf("got %d, want default cap %d", len(got), defaultAuditLimit)
	}
}
