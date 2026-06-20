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

// waitForRecords polls until the recorder holds n records or the deadline
// passes. The ledger write happens in the logging middleware after the handler
// returns, so a test that reads the response may briefly race it.
func (u *recordingUsage) waitForRecords(t *testing.T, n int) []UsageRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := u.snapshot()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records = %d after timeout, want %d", len(got), n)
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

	got := rec.waitForRecords(t, 1)
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

	got := rec.waitForRecords(t, 1)
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

	got := rec.waitForRecords(t, 1)
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
