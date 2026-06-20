package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/db"
	"github.com/orchestra-hq/atlas/internal/server"
)

// G13 (usage metering, M1 phase 6) at the integration level: the real SQLite
// ledger, the production usageRecorder bridge, a real gateway, and the
// `atlas usage` CLI reading the same store. The unit-level pieces live in
// internal/db (grouping/sums/durability) and internal/server (what gets
// recorded, incl. the interrupted-stream estimate); this proves they compose end
// to end — requests in, correct totals out, durable across a reopen, and the
// interrupted stream recorded non-zero.

// brokenStreamer streams some text then fails before Done, modeling a worker
// drop mid-stream so the gateway must record the partial output (review finding).
type brokenStreamer struct{}

func (brokenStreamer) Execute(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errors.New("not used")
}

func (brokenStreamer) ExecuteStream(_ context.Context, _ core.Request, sink core.StreamSink) error {
	_ = sink.Text("some partial output before the cut")
	return errors.New("worker dropped mid-stream")
}

func meteredGateway(t *testing.T, store *db.DB) *httptest.Server {
	t.Helper()
	gw := server.NewGateway(keyAuth{db: store}, []server.Model{
		{Name: "model-a", Exec: fixedExecutor{}, ContextWindow: 4096},
		{Name: "model-b", Exec: fixedExecutor{}, ContextWindow: 4096},
		{Name: "model-stream", Exec: brokenStreamer{}, ContextWindow: 4096},
	}, nil)
	gw.SetUsageRecorder(usageRecorder{db: store})
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func sendMessage(t *testing.T, srv *httptest.Server, key, model string, stream bool) {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":16,"stream":` + boolStr(stream) +
		`,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", key)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// waitForUsageRows polls the ledger until it holds at least n rows (the gateway
// writes them just after the response returns).
func waitForUsageRows(t *testing.T, store *db.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := store.UsageByKey(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, r := range rows {
			total += r.Requests
		}
		if total >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage rows = %d after timeout, want %d", total, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestG13_UsageMetering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := openStateDB(dir)
	if err != nil {
		t.Fatal(err)
	}

	keyA, ka, err := store.CreateKey(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	keyB, kb, err := store.CreateKey(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	srv := meteredGateway(t, store)

	// keyA: 2× model-a, 1× model-b. keyB: 1× model-a. fixedExecutor reports
	// usage {1,1} per request, so totals are predictable.
	sendMessage(t, srv, keyA, "model-a", false)
	sendMessage(t, srv, keyA, "model-a", false)
	sendMessage(t, srv, keyA, "model-b", false)
	sendMessage(t, srv, keyB, "model-a", false)
	waitForUsageRows(t, store, 4)

	idx := usageIndexer(t)

	// Per-key totals.
	byKey := idx(store.UsageByKey(ctx))
	if got := byKey[ka.ID]; got.Requests != 3 || got.InputTokens != 3 || got.OutputTokens != 3 {
		t.Errorf("key A total = %+v, want {3,3,3}", got)
	}
	if got := byKey[kb.ID]; got.Requests != 1 || got.InputTokens != 1 || got.OutputTokens != 1 {
		t.Errorf("key B total = %+v, want {1,1,1}", got)
	}

	// Per-model totals.
	byModel := idx(store.UsageByModel(ctx))
	if got := byModel["model-a"]; got.Requests != 3 || got.OutputTokens != 3 {
		t.Errorf("model-a total = %+v, want 3 reqs / 3 out", got)
	}
	if got := byModel["model-b"]; got.Requests != 1 || got.OutputTokens != 1 {
		t.Errorf("model-b total = %+v, want 1 req / 1 out", got)
	}

	// Per-worker: all served by the in-process worker.
	byWorker := idx(store.UsageByWorker(ctx))
	if got := byWorker["local"]; got.Requests != 4 || got.InputTokens != 4 || got.OutputTokens != 4 {
		t.Errorf("worker local total = %+v, want {4,4,4}", got)
	}

	// `atlas usage --json` over the same store reports the same numbers.
	out := runUsageJSON(t, dir)
	if got := rowFor(out.ByModel, "model-a"); got.Requests != 3 || got.OutputTokens != 3 {
		t.Errorf("CLI by-model model-a = %+v, want 3/3", got)
	}
	if got := rowFor(out.ByKey, ka.ID); got.Requests != 3 {
		t.Errorf("CLI by-key key A = %+v, want 3 requests", got)
	}

	// Durability: reopen the store and confirm the totals survive a restart.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := idx(reopened.UsageByWorker(ctx))["local"]; got.Requests != 4 {
		t.Errorf("after reopen worker total = %+v, want 4 requests", got)
	}
}

// TestG13_InterruptedStreamRecordsPartialUsage is the G13 interrupted case end to
// end: a stream cut off partway records the output emitted before the cut.
func TestG13_InterruptedStreamRecordsPartialUsage(t *testing.T) {
	ctx := context.Background()
	store, err := openStateDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	key, _, err := store.CreateKey(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	srv := meteredGateway(t, store)
	sendMessage(t, srv, key, "model-stream", true) // streaming, fails mid-way
	waitForUsageRows(t, store, 1)

	idx := usageIndexer(t)
	if got := idx(store.UsageByModel(ctx))["model-stream"]; got.OutputTokens <= 0 {
		t.Errorf("interrupted stream recorded %d output tokens, want > 0", got.OutputTokens)
	}
}

// usageIndexer returns a helper that indexes a usage query's result by group,
// failing the test on a query error. Its (totals, err) signature lets callers
// pass a query call directly: idx(store.UsageByKey(ctx)).
func usageIndexer(t *testing.T) func([]db.UsageTotal, error) map[string]db.UsageTotal {
	return func(totals []db.UsageTotal, err error) map[string]db.UsageTotal {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		m := make(map[string]db.UsageTotal, len(totals))
		for _, tot := range totals {
			m[tot.Group] = tot
		}
		return m
	}
}

func runUsageJSON(t *testing.T, dir string) usageReport {
	t.Helper()
	cmd := newUsageCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--state-dir", dir, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("atlas usage --json: %v", err)
	}
	var rep usageReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("decode usage JSON: %v\n%s", err, buf.String())
	}
	return rep
}

func rowFor(rows []usageRow, group string) usageRow {
	for _, r := range rows {
		if r.Group == group {
			return r
		}
	}
	return usageRow{}
}
