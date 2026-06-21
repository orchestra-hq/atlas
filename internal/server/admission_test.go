package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
)

// fixedReplicas builds a replica-count source the admission controller reads for
// capacity; the returned setter changes the live count to simulate workers joining
// or leaving.
func fixedReplicas(n int) (func(string) int, func(int)) {
	var mu sync.Mutex
	count := n
	get := func(string) int {
		mu.Lock()
		defer mu.Unlock()
		return count
	}
	set := func(v int) {
		mu.Lock()
		count = v
		mu.Unlock()
	}
	return get, set
}

func newTestAdmission(t *testing.T, cfg AdmissionConfig, replicas func(string) int) *Admission {
	t.Helper()
	a := NewAdmission(cfg)
	a.replicas = replicas
	return a
}

// TestAdmission_disabledPassesThrough: PerReplica <= 0 forwards everything with a
// no-op release and never sheds (M1 behavior).
func TestAdmission_disabledPassesThrough(t *testing.T) {
	get, _ := fixedReplicas(0) // even with zero replicas, disabled never sheds
	a := newTestAdmission(t, AdmissionConfig{PerReplica: 0}, get)
	for i := 0; i < 100; i++ {
		release, apiErr := a.Acquire(context.Background(), "m")
		if apiErr != nil {
			t.Fatalf("disabled admission shed a request: %v", apiErr)
		}
		release()
	}
}

// TestAdmission_admitsUpToCapacity then sheds 429 once the slots and queue are
// full, and frees capacity on release.
func TestAdmission_admitsUpToCapacityThenSheds(t *testing.T) {
	get, _ := fixedReplicas(2)
	// capacity = 2 replicas × 2 = 4; queue length 0 so the 5th sheds immediately.
	a := newTestAdmission(t, AdmissionConfig{PerReplica: 2, QueueLen: 0, MaxWait: time.Second, RetryAfter: 3}, get)

	var releases []func()
	for i := 0; i < 4; i++ {
		release, apiErr := a.Acquire(context.Background(), "m")
		if apiErr != nil {
			t.Fatalf("request %d shed below capacity: %v", i, apiErr)
		}
		releases = append(releases, release)
	}

	// Capacity full, queue length 0 → shed 429.
	release, apiErr := a.Acquire(context.Background(), "m")
	if apiErr == nil {
		release()
		t.Fatal("5th request admitted past capacity; want a shed")
	}
	if apiErr.Status != http.StatusTooManyRequests || apiErr.Type != anthropic.ErrRateLimit {
		t.Fatalf("shed = %d/%s, want 429/rate_limit_error", apiErr.Status, apiErr.Type)
	}
	if apiErr.RetryAfter != 3 {
		t.Fatalf("Retry-After = %d, want 3", apiErr.RetryAfter)
	}

	// Free one slot; the next request is admitted again.
	releases[0]()
	release, apiErr = a.Acquire(context.Background(), "m")
	if apiErr != nil {
		t.Fatalf("request shed after a slot freed: %v", apiErr)
	}
	release()
}

// TestAdmission_zeroCapacitySheds529: no live replica → 529 overloaded.
func TestAdmission_zeroCapacitySheds529(t *testing.T) {
	get, _ := fixedReplicas(0)
	a := newTestAdmission(t, AdmissionConfig{PerReplica: 4, QueueLen: 4, MaxWait: time.Second, RetryAfter: 2}, get)

	release, apiErr := a.Acquire(context.Background(), "m")
	if apiErr == nil {
		release()
		t.Fatal("admitted with zero replicas; want a 529 shed")
	}
	if apiErr.Status != statusOverloaded || apiErr.Type != anthropic.ErrOverloaded {
		t.Fatalf("shed = %d/%s, want 529/overloaded_error", apiErr.Status, apiErr.Type)
	}
	if apiErr.RetryAfter != 2 {
		t.Fatalf("Retry-After = %d, want 2", apiErr.RetryAfter)
	}
}

// TestAdmission_queuedRequestPromotedOnRelease: a request that finds every slot
// busy waits in the queue and is admitted when a slot frees.
func TestAdmission_queuedRequestPromotedOnRelease(t *testing.T) {
	get, _ := fixedReplicas(1)
	a := newTestAdmission(t, AdmissionConfig{PerReplica: 1, QueueLen: 4, MaxWait: 2 * time.Second}, get)

	// Fill the single slot.
	release, apiErr := a.Acquire(context.Background(), "m")
	if apiErr != nil {
		t.Fatalf("first request shed: %v", apiErr)
	}

	// A second request must queue, then be admitted once the first releases.
	admitted := make(chan *anthropic.Error, 1)
	go func() {
		rel, err := a.Acquire(context.Background(), "m")
		if err == nil {
			rel()
		}
		admitted <- err
	}()

	// Give the goroutine time to enqueue, then free the slot.
	time.Sleep(50 * time.Millisecond)
	release()

	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("queued request shed instead of being promoted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request was never promoted after a slot freed")
	}
}

// TestAdmission_maxWaitSheds: a queued request that waits past MaxWait sheds 429.
func TestAdmission_maxWaitSheds(t *testing.T) {
	get, _ := fixedReplicas(1)
	a := newTestAdmission(t, AdmissionConfig{PerReplica: 1, QueueLen: 4, MaxWait: 30 * time.Millisecond, RetryAfter: 1}, get)

	// Fill the slot and never release, so the next request waits then sheds.
	if _, apiErr := a.Acquire(context.Background(), "m"); apiErr != nil {
		t.Fatalf("first request shed: %v", apiErr)
	}
	release, apiErr := a.Acquire(context.Background(), "m")
	if apiErr == nil {
		release()
		t.Fatal("queued request admitted; want a max-wait shed")
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("max-wait shed = %d, want 429", apiErr.Status)
	}
}

// TestAdmission_capacityTracksReplicas: capacity follows the live replica count, so
// a replica leaving lowers the ceiling and a replica joining raises it.
func TestAdmission_capacityTracksReplicas(t *testing.T) {
	get, set := fixedReplicas(1)
	a := newTestAdmission(t, AdmissionConfig{PerReplica: 1, QueueLen: 0, MaxWait: time.Second}, get)

	// One replica, one slot: admit one, shed the next.
	r1, apiErr := a.Acquire(context.Background(), "m")
	if apiErr != nil {
		t.Fatalf("first request shed: %v", apiErr)
	}
	if _, err := a.Acquire(context.Background(), "m"); err == nil {
		t.Fatal("second request admitted at capacity 1")
	}

	// A second replica joins: capacity rises to 2, so another is admitted.
	set(2)
	r2, apiErr := a.Acquire(context.Background(), "m")
	if apiErr != nil {
		t.Fatalf("request shed after a replica joined: %v", apiErr)
	}
	r1()
	r2()
}
