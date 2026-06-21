package server

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
)

// defaultRetryAfter is the Retry-After (seconds) advertised on a shed 429/529 when
// none is configured.
const defaultRetryAfter = 1

// AdmissionConfig tunes the per-model admission controller (ADR-0010). PerReplica
// is the concurrency each replica is assumed to handle, so a model's ceiling is
// PerReplica × its live replica count. QueueLen bounds the FIFO of waiters past
// that ceiling; MaxWait bounds how long a waiter blocks before shedding; RetryAfter
// (seconds) is advertised on the shed response. PerReplica <= 0 disables admission
// entirely — every request is forwarded, M1's behavior (build-plan §5).
type AdmissionConfig struct {
	PerReplica int
	QueueLen   int
	MaxWait    time.Duration
	RetryAfter int
}

// Admission caps per-model in-flight concurrency, queueing then shedding load
// beyond the fleet's capacity for a model (ADR-0010). It is gateway-side and
// entirely in-memory: a control-plane restart drops queued (un-acked) requests,
// which clients retry. Capacity is read live, so it tracks workers joining and
// leaving. All methods are safe on a nil *Admission (admission disabled).
type Admission struct {
	cfg AdmissionConfig

	// replicas yields a model's current live replica count; wired by the gateway in
	// SetAdmission so capacity follows the fleet. metrics is the same instrumentation
	// the gateway feeds, used for the queue-depth gauge and shed counters.
	replicas func(model string) int
	metrics  *Metrics

	mu     sync.Mutex
	models map[string]*modelAdmit
}

// modelAdmit is one model's admission state: the count of admitted (in-flight)
// requests and the FIFO queue of waiters blocked for a slot (oldest at the front).
type modelAdmit struct {
	admitted int
	queue    *list.List // of *admitWaiter
}

// admitWaiter is one request blocked in a model's queue. ready is closed when the
// waiter is granted a freed slot; el is its position in the queue, set to nil once
// removed so a timing-out waiter can detect it lost the race to a promotion.
type admitWaiter struct {
	ready chan struct{}
	el    *list.Element
}

// NewAdmission builds an admission controller from cfg. Attach it to a gateway with
// SetAdmission, which wires the live replica-count source and the metrics sink.
func NewAdmission(cfg AdmissionConfig) *Admission {
	return &Admission{cfg: cfg, models: make(map[string]*modelAdmit)}
}

// enabled reports whether admission gates anything. Disabled (or a nil controller)
// makes every Acquire a pass-through, restoring M1's forward-everything path.
func (a *Admission) enabled() bool { return a != nil && a.cfg.PerReplica > 0 }

// retryAfterSecs is the Retry-After value for a shed response, safe on nil.
func (a *Admission) retryAfterSecs() int {
	if a == nil || a.cfg.RetryAfter <= 0 {
		return defaultRetryAfter
	}
	return a.cfg.RetryAfter
}

// Acquire reserves a concurrency slot for a request to model, blocking in a bounded
// queue when the model is at capacity and shedding with a retryable error when it
// cannot be admitted. On success it returns a release that frees the slot (and
// promotes the next waiter); on a shed it returns a nil release and the
// *anthropic.Error to write — a 429 rate_limit_error (capacity exists but is
// momentarily full) or a 529 overloaded_error (no live replica). When admission is
// disabled it is a pass-through. Safe on a nil receiver.
func (a *Admission) Acquire(ctx context.Context, model string) (func(), *anthropic.Error) {
	if !a.enabled() {
		return noopRelease, nil
	}
	capacity := a.cfg.PerReplica * a.replicas(model)

	a.mu.Lock()
	ma := a.models[model]
	if ma == nil {
		ma = &modelAdmit{queue: list.New()}
		a.models[model] = ma
	}
	switch {
	case capacity <= 0:
		// No live replica can serve it right now (e.g. it dropped between resolution
		// and admission): overloaded, retryable.
		a.mu.Unlock()
		a.metrics.incShed(model, "529")
		return nil, overloadedErr(a.retryAfterSecs())
	case ma.admitted < capacity:
		ma.admitted++
		a.mu.Unlock()
		return a.releaseSlot(model), nil
	case ma.queue.Len() >= a.cfg.QueueLen:
		// Queue is full: capacity exists but is momentarily saturated.
		a.mu.Unlock()
		a.metrics.incShed(model, "429")
		return nil, rateLimitedErr(a.retryAfterSecs())
	}
	// Enqueue and wait for a freed slot, the max wait, or ctx cancel.
	wt := &admitWaiter{ready: make(chan struct{})}
	wt.el = ma.queue.PushBack(wt)
	a.metrics.setQueueDepth(model, ma.queue.Len())
	a.mu.Unlock()

	timer := time.NewTimer(a.cfg.MaxWait)
	defer timer.Stop()
	select {
	case <-wt.ready:
		// Promoted: a release transferred its slot to us (admitted already counts it).
		return a.releaseSlot(model), nil
	case <-timer.C:
		return a.abandon(model, wt)
	case <-ctx.Done():
		return a.abandon(model, wt)
	}
}

// abandon removes a timed-out or cancelled waiter from the queue and sheds a 429. It
// loses a race with a concurrent promotion: if the waiter was already granted a slot
// (no longer queued), abandon honors that slot and returns a release instead of
// shedding, so a transferred slot is never leaked.
func (a *Admission) abandon(model string, wt *admitWaiter) (func(), *anthropic.Error) {
	a.mu.Lock()
	ma := a.models[model]
	if wt.el != nil && ma != nil {
		ma.queue.Remove(wt.el)
		wt.el = nil
		a.metrics.setQueueDepth(model, ma.queue.Len())
		a.mu.Unlock()
		a.metrics.incShed(model, "429")
		return nil, rateLimitedErr(a.retryAfterSecs())
	}
	// Promoted between our wake and this lock: we hold a slot, honor it.
	a.mu.Unlock()
	return a.releaseSlot(model), nil
}

// releaseSlot returns a single-shot release that frees one admitted slot for model
// and promotes the next waiter (transferring the slot rather than dropping it).
func (a *Admission) releaseSlot(model string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			ma := a.models[model]
			if ma == nil {
				return
			}
			ma.admitted--
			a.promoteLocked(model, ma)
		})
	}
}

// promoteLocked grants freed capacity to queued waiters, oldest first, up to the
// model's current capacity (re-read, so a shrunk fleet promotes fewer). Each
// promotion keeps the admitted count — the slot moves from the releaser to the
// waiter. The caller holds mu.
func (a *Admission) promoteLocked(model string, ma *modelAdmit) {
	capacity := a.cfg.PerReplica * a.replicas(model)
	for ma.queue.Len() > 0 && ma.admitted < capacity {
		front := ma.queue.Front()
		wt := front.Value.(*admitWaiter)
		ma.queue.Remove(front)
		wt.el = nil
		ma.admitted++
		close(wt.ready)
	}
	a.metrics.setQueueDepth(model, ma.queue.Len())
}
