package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
)

// recordingUsage is a test UsageRecorder that captures every record written, so
// tests can assert what the gateway billed.
type recordingUsage struct {
	mu      sync.Mutex
	records []UsageRecord
}

func (u *recordingUsage) Record(_ context.Context, rec UsageRecord) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records = append(u.records, rec)
	return nil
}

func (u *recordingUsage) snapshot() []UsageRecord {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]UsageRecord(nil), u.records...)
}

// waitForRecord polls until the recorder holds at least one record or the
// deadline passes. The ledger write happens in the logging middleware after the
// handler returns, so a test that reads the response may briefly race it.
func (u *recordingUsage) waitForRecord(t *testing.T) []UsageRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := u.snapshot()
		if len(got) >= 1 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("no usage records after timeout, want at least 1")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// meteredServer builds a gateway serving one model via exec, with usage metering
// attached, and returns the test server plus the recorder.
func meteredServer(t *testing.T, exec Executor) (*httptest.Server, *recordingUsage) {
	t.Helper()
	rec := &recordingUsage{}
	g := NewGateway(staticAuth(testKey), []Model{{Name: testModel, Exec: exec, ContextWindow: 4096}}, nil)
	g.SetUsageRecorder(rec)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestUsageRecordedOnSuccess(t *testing.T) {
	srv, rec := meteredServer(t, &echoExecutor{reply: "hi", outToken: 11})

	resp, _ := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := rec.waitForRecord(t)
	r := got[0]
	if r.KeyID != "test" || r.Model != testModel || r.WorkerID != localWorkerID {
		t.Errorf("record identity = %+v, want key=test model=%s worker=%s", r, testModel, localWorkerID)
	}
	// Token counts must match the engine's usage (the same values the response
	// echoes — the G13 consistency requirement).
	if r.InputTokens != 7 || r.OutputTokens != 11 {
		t.Errorf("record tokens = (%d,%d), want (7,11)", r.InputTokens, r.OutputTokens)
	}
}

func TestUsageRecordedOnStream(t *testing.T) {
	srv, rec := meteredServer(t, &streamExecutor{deltas: []string{"a", "b", "c"}})

	resp, _ := streamPost(t, srv, `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := rec.waitForRecord(t)
	// streamExecutor.Done reports usage {4, len(deltas)=3}.
	if got[0].InputTokens != 4 || got[0].OutputTokens != 3 {
		t.Errorf("stream record tokens = (%d,%d), want (4,3)", got[0].InputTokens, got[0].OutputTokens)
	}
}

func TestUsageNotRecordedOnAuthFailure(t *testing.T) {
	srv, rec := meteredServer(t, &echoExecutor{reply: "hi"})

	// No key: 401, and nothing billed.
	resp, _ := post(t, srv, "", `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	// Give any erroneous async write a moment to land, then assert none did.
	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("usage recorded on auth failure: %+v", got)
	}
}

func TestCountTokensNotBilled(t *testing.T) {
	srv, rec := meteredServer(t, &countingExecutor{count: 9})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/count_tokens",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", testKey)
	req.Header.Set("content-type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count_tokens status = %d", resp.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("count_tokens was billed: %+v", got)
	}
}

// TestInterruptedStreamRecordsPartialUsage is the unit-level G13 interrupted
// case: a stream that emits some text then fails records the output emitted so
// far, not zero.
func TestInterruptedStreamRecordsPartialUsage(t *testing.T) {
	srv, rec := meteredServer(t, &interruptingStreamer{deltas: []string{"hello ", "world "}})

	// The client receives a partial stream ending in an error event; the request
	// itself does not fail at the HTTP level (headers were already sent).
	resp, _ := streamPost(t, srv, `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := rec.waitForRecord(t)
	if got[0].OutputTokens <= 0 {
		t.Errorf("interrupted stream recorded %d output tokens, want > 0 (the bytes emitted before the cut)", got[0].OutputTokens)
	}
}

// interruptingStreamer emits its deltas, then fails before Done — modeling a
// worker drop or client-write failure mid-stream.
type interruptingStreamer struct{ deltas []string }

func (i *interruptingStreamer) Execute(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errors.New("not used")
}

func (i *interruptingStreamer) ExecuteStream(_ context.Context, _ core.Request, sink core.StreamSink) error {
	for _, d := range i.deltas {
		if err := sink.Text(d); err != nil {
			return err
		}
	}
	return errors.New("worker dropped mid-stream")
}

// TestInterruptedStreamRecordsInputTokens covers the input half of the G13
// interrupted case: the engine reports usage only on a clean completion, so an
// interrupted stream must attribute input from the prompt count taken before
// dispatch rather than recording zero input.
func TestInterruptedStreamRecordsInputTokens(t *testing.T) {
	srv, rec := meteredServer(t, &countingInterruptingStreamer{deltas: []string{"hello ", "world "}, prompt: 13})

	resp, _ := streamPost(t, srv, `{"model":"test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := rec.waitForRecord(t)
	if got[0].InputTokens != 13 {
		t.Errorf("interrupted stream recorded %d input tokens, want 13 (the pre-dispatch prompt count, since the engine never reported usage)", got[0].InputTokens)
	}
	if got[0].OutputTokens <= 0 {
		t.Errorf("interrupted stream recorded %d output tokens, want > 0", got[0].OutputTokens)
	}
}

// countingInterruptingStreamer fails before Done (so the engine never reports
// usage) and counts prompt tokens — modeling a real engine on the interrupted
// path, where input usage must come from the pre-dispatch count.
type countingInterruptingStreamer struct {
	deltas []string
	prompt int
}

func (s *countingInterruptingStreamer) Execute(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errors.New("not used")
}

func (s *countingInterruptingStreamer) ExecuteStream(_ context.Context, _ core.Request, sink core.StreamSink) error {
	for _, d := range s.deltas {
		if err := sink.Text(d); err != nil {
			return err
		}
	}
	return errors.New("worker dropped mid-stream")
}

func (s *countingInterruptingStreamer) CountTokens(context.Context, core.Request) (int, error) {
	return s.prompt, nil
}

// TestUsageRecordsCanonicalModelName asserts the ledger records the canonical
// served model, not the alias a client addressed, so per-model totals group by
// the real model (internal/db.UsageRecord's documented invariant).
func TestUsageRecordsCanonicalModelName(t *testing.T) {
	rec := &recordingUsage{}
	g := NewGateway(staticAuth(testKey),
		[]Model{{Name: testModel, Exec: &echoExecutor{reply: "hi", outToken: 4}, ContextWindow: 4096}},
		map[string]string{"my-alias": testModel})
	g.SetUsageRecorder(rec)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	resp, _ := post(t, srv, testKey, `{"model":"my-alias","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := rec.waitForRecord(t)
	if got[0].Model != testModel {
		t.Errorf("usage recorded model %q, want canonical %q (not the alias the client addressed)", got[0].Model, testModel)
	}
}

// TestUsageAttributedToStableWorkerName is the unit-level M2-phase-1 worker
// identity case: usage is billed to the worker's stable --name, not the ephemeral
// per-connection id, and that attribution survives a reconnect (a fresh
// connection id for the same machine). Otherwise `atlas usage --by-worker` would
// fragment one machine's totals across every connection id it ever held
// (docs/follow-ups.md).
func TestUsageAttributedToStableWorkerName(t *testing.T) {
	rec := &recordingUsage{}
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.SetUsageRecorder(rec)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	// First connection: the machine "gpu-box" joins under connection id "w_conn1".
	g.RegisterInstance("w_conn1", "gpu-box", Model{Name: testModel, Exec: &echoExecutor{reply: "hi", outToken: 4}, ContextWindow: 4096})

	resp, _ := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := rec.waitForRecord(t)
	if got[0].WorkerID != "gpu-box" {
		t.Errorf("usage attributed to %q, want the stable worker name %q (not the connection id)", got[0].WorkerID, "gpu-box")
	}

	// The machine reconnects: its old connection tears down and it rejoins under a
	// fresh connection id "w_conn2" with the same name.
	g.UnregisterWorker("w_conn1")
	g.RegisterInstance("w_conn2", "gpu-box", Model{Name: testModel, Exec: &echoExecutor{reply: "hi", outToken: 4}, ContextWindow: 4096})

	resp, _ = post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status after reconnect = %d", resp.StatusCode)
	}
	// Both requests must attribute to the same stable name despite the new
	// connection id — no fragmentation across reconnects.
	deadline := time.Now().Add(2 * time.Second)
	for {
		records := rec.snapshot()
		if len(records) >= 2 {
			for _, r := range records {
				if r.WorkerID != "gpu-box" {
					t.Errorf("usage attributed to %q across reconnect, want %q for every record", r.WorkerID, "gpu-box")
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d usage records after timeout, want 2 (one per request)", len(records))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		bytes int
		want  int
	}{{0, 0}, {1, 1}, {3, 1}, {4, 1}, {8, 2}, {100, 25}}
	for _, c := range cases {
		if got := estimateTokens(c.bytes); got != c.want {
			t.Errorf("estimateTokens(%d) = %d, want %d", c.bytes, got, c.want)
		}
	}
}
