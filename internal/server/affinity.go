package server

import (
	"container/list"
	"hash/fnv"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/orchestra-hq/atlas/internal/core"
)

// affinitySessionHeader lets a client that tracks its own session id pin a
// conversation to a routing key explicitly, bypassing the derived prefix hash.
// The Anthropic Messages API is stateless and has no native session id (ADR-0002),
// so this is an Atlas extension a client may set but never has to.
const affinitySessionHeader = "x-atlas-session"

// defaultAffinityPrefixMessages is how many leading conversation messages join the
// system prompt in the derived routing key. The earliest messages are the stable
// shared prefix a prefix-caching engine actually reuses across a conversation's
// growing turns, so hashing them (not the whole, growing transcript) keeps the key
// stable turn to turn while still distinguishing conversations that share a system
// prompt but diverge immediately.
const defaultAffinityPrefixMessages = 2

// warmKeyCap bounds the per-fleet set of recently-routed affinity keys tracked for
// the warm-key gauge, so the observability state can never grow without limit
// however many distinct conversations pass through. The oldest key is evicted once
// the cap is reached (it is only a gauge input, not routing state — routing is the
// stateless hash, so eviction loses nothing a request depends on).
const warmKeyCap = 8192

// AffinityConfig tunes prefix/session-affinity routing (ADR-0011). Tolerance is how
// many more in-flight requests the affine replica may carry than the least-loaded
// one and still be chosen; past it, selection falls back to least-in-flight, so
// affinity never queues a request behind a busy replica that backpressure would
// otherwise spread or shed. A negative Tolerance disables affinity entirely.
// PrefixMessages is how many leading messages join the system prompt in the derived
// routing key.
type AffinityConfig struct {
	Tolerance      int
	PrefixMessages int
}

// Affinity steers a conversation to the replica that already holds its warm prefix
// cache, as a load-bounded hint on top of ADR-0010 least-in-flight selection
// (ADR-0011). It is gateway-side and stateless for routing — a request's replica is
// a pure function of its routing key and the live replica set (rendezvous hashing),
// so affinity survives a control-plane restart with no session store. The only
// state it keeps is the bounded warm-key set feeding the observability gauge.
//
// All methods are safe on a nil *Affinity (affinity disabled), so the gateway calls
// them unconditionally; a nil or disabled controller makes selectReplica fall
// straight through to least-in-flight.
type Affinity struct {
	cfg     AffinityConfig
	metrics *Metrics

	mu   sync.Mutex
	warm *warmKeys // bounded recent-key tracker behind the per-replica warm-key gauge
}

// NewAffinity builds an affinity controller from cfg. Attach it to a gateway with
// SetAffinity, which wires the metrics sink. A negative cfg.Tolerance leaves it
// disabled; PrefixMessages <= 0 is floored to the default so the routing key always
// covers at least the system prompt plus the conversation's opening.
func NewAffinity(cfg AffinityConfig) *Affinity {
	if cfg.PrefixMessages <= 0 {
		cfg.PrefixMessages = defaultAffinityPrefixMessages
	}
	return &Affinity{cfg: cfg, warm: newWarmKeys(warmKeyCap)}
}

// enabled reports whether affinity steers selection. Disabled (or a nil controller)
// makes selectReplica a pass-through to least-in-flight — pure ADR-0010 behavior.
func (af *Affinity) enabled() bool { return af != nil && af.cfg.Tolerance >= 0 }

// routingKey derives the affinity key for a request: the explicit x-atlas-session
// header when a client supplies one, otherwise a stable hash of the conversation's
// leading prefix (system prompt + the earliest messages). It returns "" when
// affinity is disabled, so the derivation cost is skipped entirely when off and the
// caller treats "" as "no affinity, use least-in-flight".
func (af *Affinity) routingKey(r *http.Request, req core.Request) string {
	if !af.enabled() {
		return ""
	}
	if s := strings.TrimSpace(r.Header.Get(affinitySessionHeader)); s != "" {
		return s
	}
	return af.prefixKey(req)
}

// prefixKey hashes the stable conversation prefix — the system prompt plus the text
// of the leading PrefixMessages messages — into a routing key. Roles are mixed in
// so a user/assistant pair cannot collide with a single combined turn, and a length
// prefix delimits each part so concatenation is unambiguous.
func (af *Affinity) prefixKey(req core.Request) string {
	h := fnv.New64a()
	writeChunk(h, "sys", req.System)
	n := af.cfg.PrefixMessages
	if n > len(req.Messages) {
		n = len(req.Messages)
	}
	for i := 0; i < n; i++ {
		m := req.Messages[i]
		writeChunk(h, string(m.Role), m.Text())
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// writeChunk feeds one length-delimited labeled part into the prefix hash, so
// distinct (label, value) splits can never alias to the same byte stream.
func writeChunk(h interface{ Write([]byte) (int, error) }, label, value string) {
	_, _ = h.Write([]byte(label))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}

// selectReplica chooses a replica for a request to model. With affinity enabled, a
// real routing key, and more than one replica, it prefers the key's affine replica
// (rendezvous hashing) — but only while that replica's in-flight count is within the
// configured tolerance of the least-loaded replica; past the tolerance it falls back
// to least-in-flight, so affinity yields to load and never undermines backpressure.
// Disabled, an empty key, or a single replica all fall straight through to
// least-in-flight (ADR-0010). Safe on a nil receiver. The caller holds the gateway's
// route lock, so rs is stable for the call.
func (af *Affinity) selectReplica(model, key string, rs []route) route {
	if !af.enabled() || key == "" || len(rs) < 2 {
		return leastInFlight(rs)
	}
	affine := rendezvous(key, rs)
	minN := int64(math.MaxInt64)
	for i := range rs {
		if n := rs[i].inflight.Load(); n < minN {
			minN = n
		}
	}
	if affine.inflight.Load() <= minN+int64(af.cfg.Tolerance) {
		af.metrics.incAffinity(model, true)
		af.recordWarm(model, key, affine.workerID)
		return affine
	}
	af.metrics.incAffinity(model, false)
	return leastInFlight(rs)
}

// rendezvous picks the route with the highest hash of (key, route identity) —
// rendezvous (highest-random-weight) hashing, which gives the consistent-hash
// property ADR-0011 calls for without a ring: adding or removing a replica only
// remaps the keys that were or now are highest-weight on it, leaving every other
// key on its current replica. The route's stable worker id is the identity, so a
// connection cycling its ephemeral id does not reshuffle keys away from a worker
// that is still serving. rs must be non-empty.
func rendezvous(key string, rs []route) route {
	best := rs[0]
	var bestH uint64
	for i := range rs {
		h := fnv.New64a()
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(rs[i].workerID))
		if hv := h.Sum64(); i == 0 || hv > bestH {
			best, bestH = rs[i], hv
		}
	}
	return best
}

// recordWarm notes that key is warm on worker for model, updating the bounded
// warm-key set and the per-replica gauge. Called only on an affinity hit — the
// event that actually warms (or re-warms) a replica's prefix cache for the key.
func (af *Affinity) recordWarm(model, key, worker string) {
	if af == nil {
		return
	}
	af.mu.Lock()
	moved, evicted := af.warm.record(model, key, worker)
	af.mu.Unlock()
	for _, c := range moved {
		af.metrics.setWarmKeys(c.model, c.worker, c.count)
	}
	for _, c := range evicted {
		af.metrics.setWarmKeys(c.model, c.worker, c.count)
	}
}

// warmKeys is a bounded LRU of the affinity keys most recently routed to each
// replica, the input to the per-replica warm-key gauge. It tracks, per (model,
// worker), the count of distinct recently-warmed keys; the oldest key is evicted
// once cap distinct keys are tracked across the fleet. It is not routing state —
// routing is the stateless rendezvous hash — so eviction only ages out a gauge
// sample, never anything a request depends on. Not safe for concurrent use; the
// Affinity mutex guards it.
type warmKeys struct {
	cap    int
	ll     *list.List // MRU at the front; values are *warmEntry
	index  map[warmKeyID]*list.Element
	counts map[modelWorker]int
}

type warmEntry struct {
	id     warmKeyID
	worker string
}

type warmKeyID struct{ model, key string }

type modelWorker struct{ model, worker string }

// warmCount is one (model, worker) gauge sample emitted after a record mutation.
type warmCount struct {
	model, worker string
	count         int
}

func newWarmKeys(limit int) *warmKeys {
	return &warmKeys{
		cap:    limit,
		ll:     list.New(),
		index:  make(map[warmKeyID]*list.Element),
		counts: make(map[modelWorker]int),
	}
}

// record registers key→worker for model as the most-recently-warmed key and returns
// the gauge samples to emit: moved covers the worker(s) whose count changed for this
// key (a new key, or a key that migrated to a different replica), evicted covers a
// worker whose oldest key was dropped to stay within cap. Counts are the post-update
// distinct-key totals.
func (wk *warmKeys) record(model, key, worker string) (moved, evicted []warmCount) {
	id := warmKeyID{model: model, key: key}
	if el, ok := wk.index[id]; ok {
		e := el.Value.(*warmEntry)
		wk.ll.MoveToFront(el)
		if e.worker == worker {
			return nil, nil // same replica, still warm: only its recency changed
		}
		old := wk.dec(model, e.worker)
		e.worker = worker
		neu := wk.inc(model, worker)
		return []warmCount{old, neu}, nil
	}
	el := wk.ll.PushFront(&warmEntry{id: id, worker: worker})
	wk.index[id] = el
	moved = []warmCount{wk.inc(model, worker)}
	if wk.ll.Len() > wk.cap {
		evicted = wk.evictOldest()
	}
	return moved, evicted
}

// evictOldest drops the least-recently-warmed key and returns its replica's updated
// gauge sample.
func (wk *warmKeys) evictOldest() []warmCount {
	back := wk.ll.Back()
	if back == nil {
		return nil
	}
	e := back.Value.(*warmEntry)
	wk.ll.Remove(back)
	delete(wk.index, e.id)
	return []warmCount{wk.dec(e.id.model, e.worker)}
}

func (wk *warmKeys) inc(model, worker string) warmCount {
	mw := modelWorker{model: model, worker: worker}
	wk.counts[mw]++
	return warmCount{model: model, worker: worker, count: wk.counts[mw]}
}

func (wk *warmKeys) dec(model, worker string) warmCount {
	mw := modelWorker{model: model, worker: worker}
	wk.counts[mw]--
	if wk.counts[mw] <= 0 {
		delete(wk.counts, mw)
		return warmCount{model: model, worker: worker, count: 0}
	}
	return warmCount{model: model, worker: worker, count: wk.counts[mw]}
}
