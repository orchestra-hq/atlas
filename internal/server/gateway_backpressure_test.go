package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/core"
)

// blockingExecutor signals when an Execute starts and blocks it until release is
// closed, so a test can pin a replica's admission slot busy on demand.
type blockingExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingExecutor) Execute(ctx context.Context, _ core.Request) (core.Response, error) {
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
		return core.Response{}, ctx.Err()
	}
	return core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock("done")},
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 5, OutputTokens: 3},
	}, nil
}

// countRecorder counts persisted usage rows (synchronous, so the count is exact
// once a handler returns).
type countRecorder struct {
	mu sync.Mutex
	n  int
}

func (c *countRecorder) Record(context.Context, UsageRecord) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

func (c *countRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

const msgBody = `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

func postMessages(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(msgBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestBackpressure_shedsWhenSaturated is the unit-level G16 case: it drives a
// single-slot model past capacity: one request holds the slot, one queues, and a
// third is shed with a well-formed retryable 429 carrying Retry-After — never a 5xx
// or a hang. The admitted and queued requests then complete 200, and usage records
// only the two that ran (the shed request is not billable). The shed path, queue
// depth, and shed counter are exercised end to end.
func TestBackpressure_shedsWhenSaturated(t *testing.T) {
	exec := &blockingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	usage := &countRecorder{}
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.RegisterInstance("w1", "w1", Model{Name: "m", Exec: exec})
	g.SetUsageRecorder(usage)
	g.SetMetrics(NewMetrics())
	// capacity = 1 replica × 1 = 1; queue holds 1; a contrary request sheds 429.
	g.SetAdmission(NewAdmission(AdmissionConfig{PerReplica: 1, QueueLen: 1, MaxWait: 2 * time.Second, RetryAfter: 5}))

	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	// Request A acquires the only slot and blocks in Execute.
	aDone := make(chan *http.Response, 1)
	go func() { aDone <- postMessages(t, srv.URL) }()
	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("request A never reached the executor")
	}

	// Request B fills the queue (one waiter) but cannot run while A holds the slot.
	bDone := make(chan *http.Response, 1)
	go func() { bDone <- postMessages(t, srv.URL) }()
	// Let B enqueue before C arrives. Poll the queue-depth gauge rather than sleep.
	waitForCond(t, func() bool { return g.metrics.Snapshot().QueueDepth >= 1 }, "request B enqueued")

	// Request C finds the queue full and is shed immediately with a 429.
	cResp := postMessages(t, srv.URL)
	if cResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("shed status = %d, want 429", cResp.StatusCode)
	}
	if ra := cResp.Header.Get("Retry-After"); ra != "5" {
		t.Fatalf("Retry-After = %q, want \"5\"", ra)
	}
	_ = cResp.Body.Close()
	if snap := g.metrics.Snapshot(); snap.Shed < 1 {
		t.Fatalf("shed counter = %d, want >= 1", snap.Shed)
	}

	// Release the executor; A completes, and B is promoted off the queue and runs.
	go func() {
		<-exec.started // B's Execute, once promoted
	}()
	close(exec.release)

	for _, ch := range []chan *http.Response{aDone, bDone} {
		select {
		case resp := <-ch:
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("admitted request status = %d, want 200", resp.StatusCode)
			}
			_ = resp.Body.Close()
		case <-time.After(3 * time.Second):
			t.Fatal("an admitted request never completed")
		}
	}

	// Only the two requests that actually ran are billable; the shed one is not.
	if got := usage.count(); got != 2 {
		t.Fatalf("usage recorded %d requests, want 2 (shed C is not billable)", got)
	}
}
