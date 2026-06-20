package db

import (
	"context"
	"path/filepath"
	"testing"
)

// openTemp opens a fresh store in a temp dir for a test.
func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "atlas.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func mustRecord(t *testing.T, d *DB, u UsageRecord) {
	t.Helper()
	if err := d.RecordUsage(context.Background(), u); err != nil {
		t.Fatalf("record usage: %v", err)
	}
}

// byGroup indexes totals by their group value for order-independent assertions.
func byGroup(totals []UsageTotal) map[string]UsageTotal {
	m := make(map[string]UsageTotal, len(totals))
	for _, t := range totals {
		m[t.Group] = t
	}
	return m
}

func TestUsageGroupsAndSums(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()

	// Two keys, two models, one worker. Totals must sum within each grouping.
	mustRecord(t, d, UsageRecord{KeyID: "key_a", Model: "m1", WorkerID: "w1", InputTokens: 10, OutputTokens: 5})
	mustRecord(t, d, UsageRecord{KeyID: "key_a", Model: "m2", WorkerID: "w1", InputTokens: 3, OutputTokens: 7})
	mustRecord(t, d, UsageRecord{KeyID: "key_b", Model: "m1", WorkerID: "w1", InputTokens: 100, OutputTokens: 1})

	keys, err := d.UsageByKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bk := byGroup(keys)
	if got := bk["key_a"]; got.Requests != 2 || got.InputTokens != 13 || got.OutputTokens != 12 {
		t.Errorf("key_a total = %+v, want {2 reqs, 13 in, 12 out}", got)
	}
	if got := bk["key_b"]; got.Requests != 1 || got.InputTokens != 100 || got.OutputTokens != 1 {
		t.Errorf("key_b total = %+v, want {1 req, 100 in, 1 out}", got)
	}

	models, err := d.UsageByModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bm := byGroup(models)
	if got := bm["m1"]; got.Requests != 2 || got.InputTokens != 110 || got.OutputTokens != 6 {
		t.Errorf("m1 total = %+v, want {2 reqs, 110 in, 6 out}", got)
	}
	if got := bm["m2"]; got.Requests != 1 || got.InputTokens != 3 || got.OutputTokens != 7 {
		t.Errorf("m2 total = %+v, want {1 req, 3 in, 7 out}", got)
	}

	workers, err := d.UsageByWorker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bw := byGroup(workers)
	if got := bw["w1"]; got.Requests != 3 || got.InputTokens != 113 || got.OutputTokens != 13 {
		t.Errorf("w1 total = %+v, want {3 reqs, 113 in, 13 out}", got)
	}
}

func TestUsageOrdersBySpendDescending(t *testing.T) {
	d := openTemp(t)
	mustRecord(t, d, UsageRecord{KeyID: "small", Model: "m", WorkerID: "w", InputTokens: 1, OutputTokens: 1})
	mustRecord(t, d, UsageRecord{KeyID: "big", Model: "m", WorkerID: "w", InputTokens: 500, OutputTokens: 500})

	keys, err := d.UsageByKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Group != "big" {
		t.Errorf("usage by key = %+v, want the biggest spender first", keys)
	}
}

func TestUsageEmptyLedger(t *testing.T) {
	d := openTemp(t)
	totals, err := d.UsageByKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 0 {
		t.Errorf("empty ledger usage = %+v, want none", totals)
	}
}

func TestUsageSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRecord(t, d, UsageRecord{KeyID: "k", Model: "m", WorkerID: "w", InputTokens: 42, OutputTokens: 8})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same file: the row is durable (G13 "records survive restart").
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d2.Close() }()
	totals, err := d2.UsageByKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].InputTokens != 42 || totals[0].OutputTokens != 8 {
		t.Errorf("after reopen usage = %+v, want the one persisted row", totals)
	}
}

// TestUsageCoexistsWithKeys guards that the v2 migration leaves the v1 api_keys
// table intact — both tables work in one store after the incremental migrate.
func TestUsageCoexistsWithKeys(t *testing.T) {
	d := openTemp(t)
	ctx := context.Background()
	if _, _, err := d.CreateKey(ctx, nil, false); err != nil {
		t.Fatalf("create key after v2 migration: %v", err)
	}
	mustRecord(t, d, UsageRecord{KeyID: "k", Model: "m", WorkerID: "w", InputTokens: 1, OutputTokens: 1})
	keys, err := d.ListKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys after usage write = %v (err %v), want 1", keys, err)
	}
}
