package server

import (
	"strconv"
	"testing"
)

// benchRoutes builds n routes with stable worker ids and zeroed in-flight counts,
// the shape rendezvous and selectReplica hash and rank over.
func benchRoutes(n int) []route {
	rs := make([]route, n)
	for i := range rs {
		rs[i] = mkRoute("worker-"+strconv.Itoa(i), 0)
	}
	return rs
}

// BenchmarkRendezvous measures the per-replica hashing cost on the dispatch hot
// path: one call ranks every replica for a key. allocs/op is the figure of merit —
// a fresh hasher per replica shows up here.
func BenchmarkRendezvous(b *testing.B) {
	rs := benchRoutes(8)
	const key = "x-atlas-session-abcdef0123456789"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = rendezvous(key, rs)
	}
}

// BenchmarkSelectReplicaParallel measures concurrent dispatch selection with
// affinity enabled and every pick a hit, so recordWarm runs on every iteration.
// Dispatch holds only the gateway read lock, so these calls run in parallel and
// contend on the affinity mutex — this is the serialization the refinement targets.
func BenchmarkSelectReplicaParallel(b *testing.B) {
	af := NewAffinity(AffinityConfig{Tolerance: 0})
	rs := benchRoutes(8)
	// A modest key space: repeats exercise the warm "same replica" fast path,
	// while turnover drives new-key inserts and eviction, as live traffic mixes.
	keys := make([]string, 256)
	for i := range keys {
		keys[i] = "conv-" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = af.selectReplica("m", keys[i&255], rs)
			i++
		}
	})
}
