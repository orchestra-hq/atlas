package server

import (
	"sync"
	"testing"
)

// TestHub_workerModelsConcurrentSnapshot exercises dynamic add/remove of a
// worker's models against concurrent Workers() snapshots. Workers() returns
// shallow WorkerInfo copies whose Models slice aliases the hub's backing array,
// so an in-place mutation races a reader iterating that slice. Churning a middle
// element forces remove to shift the tail in place; multiple readers iterating
// each snapshot widen the window. Fails under `go test -race` unless
// add/removeWorkerModel are copy-on-write.
func TestHub_workerModelsConcurrentSnapshot(t *testing.T) {
	h := NewHub("tok", nil)
	h.mu.Lock()
	h.workers["w1"] = &hubWorker{info: WorkerInfo{ID: "w1"}}
	h.mu.Unlock()
	for _, m := range []string{"m0", "m1", "m2", "m3", "m4"} {
		h.addWorkerModel("w1", m)
	}

	const iters = 5000
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: churn a middle element so remove shifts the tail in place.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			h.removeWorkerModel("w1", "m2")
			h.addWorkerModel("w1", "m2")
		}
		close(stop)
	}()

	// Readers: snapshot, then iterate the aliased slice repeatedly.
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, w := range h.Workers() {
					for pass := 0; pass < 8; pass++ {
						for _, m := range w.Models { // read outside any lock
							_ = m
						}
					}
				}
			}
		}()
	}

	wg.Wait()

	// The churn ends on an add, so the worker is back to its five models intact.
	got := h.Workers()
	if len(got) != 1 || len(got[0].Models) != 5 {
		t.Fatalf("final models = %v, want one worker with 5 models", got)
	}
}
