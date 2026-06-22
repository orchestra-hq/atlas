// Package server is the control plane: the client-facing gateway (auth,
// routing, and — from later phases — SSE), plus the worker registry and
// scheduler that stay trivial in M0 single-node mode (ADR-0003).
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/api/anthropic"
	"github.com/orchestra-hq/atlas/internal/core"
)

// statusOverloaded is the non-standard 529 status Anthropic uses for transient
// overload; net/http has no constant for it. SDK retry logic keys on it.
const statusOverloaded = 529

// Executor runs one inference request to completion. It is the gateway's view
// of a worker: in M0 the single in-process worker implements it directly (the
// wire protocol for remote workers is an M1 decision — ADR build-note 4).
type Executor interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
}

// StreamExecutor is an Executor that can also stream a response incrementally.
// The in-process worker implements it; when an executor does not, the gateway
// falls back to buffering a non-streaming Execute and replaying it as a stream,
// so the SSE surface holds regardless of engine capability.
type StreamExecutor interface {
	Executor
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
}

// TokenCounter is an Executor that can count a request's prompt tokens using
// the engine's real tokenizer. The gateway uses it for POST
// /v1/messages/count_tokens and to assert context-window fit before dispatch
// (docs/m0-acceptance.md). The in-process worker implements it; an executor
// that does not simply skips the pre-dispatch assertion.
type TokenCounter interface {
	CountTokens(ctx context.Context, req core.Request) (int, error)
}

// Embedder is an executor that can serve the embedding model class (M3 phase 2a,
// ADR-0012): text in, vectors out, single-shot. Both the in-process worker and the
// remote worker implement it, so POST /v1/embeddings dispatches to either exactly as
// the chat surfaces dispatch Execute. A route whose executor does not implement it
// (a chat model addressed via the embeddings endpoint) is rejected by the gateway's
// class check before it ever reaches here.
type Embedder interface {
	Embed(ctx context.Context, req core.EmbedRequest) (core.EmbedResponse, error)
}

// Reranker is an executor that can serve the reranker model class (M3 phase 2b,
// ADR-0012): query + documents in, relevance-ordered results out, single-shot. Both
// the in-process and remote workers implement it, so POST /v1/rerank dispatches to
// either exactly as the chat surfaces dispatch Execute. A route whose executor does
// not implement it is rejected by the gateway's class check before reaching here.
type Reranker interface {
	Rerank(ctx context.Context, req core.RerankRequest) (core.RerankResponse, error)
}

// Autostarter brings a model online on demand and tracks its activity (M1 phase
// 4b-2). The gateway calls EnsureModel when a request names a catalog model with
// no live route — it deploys one replica and blocks until an instance is ready —
// and Touch on every request that routes, so an actively used auto-started model
// is not idle-stopped. The scheduler implements it; nil disables auto-start
// (single-node mode and tests), leaving an unrouted model a plain 404.
type Autostarter interface {
	EnsureModel(ctx context.Context, model string) bool
	Touch(model string)
}

// Identity is the authenticated caller behind a request: which key it is, the
// model names it may use (empty = all), and whether it carries the admin scope
// the /admin/* surface requires (phase 5b). The gateway gets it from the
// Authenticator on every request.
type Identity struct {
	KeyID     string
	Allowlist []string
	Admin     bool
}

// Authenticator validates a presented API-key secret (from x-api-key or
// Authorization: Bearer). ok is false for an unknown or revoked key (the
// gateway answers 401); a non-nil error is an auth-backend failure (500). It is
// consulted on every client request, with no caching, so a revoked key stops
// working immediately (ADR-0008). The concrete implementation is the SQLite key
// store (internal/db) bridged in the CLI; tests supply a static stub.
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) (id Identity, ok bool, err error)
}

// Model is one model the gateway serves: a canonical Name, the Executor that
// runs it, and its ContextWindow in tokens (0 = unknown, assertion skipped).
// Class is the model class (M3 phase 2a, ADR-0012); empty is treated as "chat", so
// every pre-class model and raw spec routes to the chat surface unchanged.
type Model struct {
	Name          string
	Exec          Executor
	ContextWindow int
	Class         string
}

// ClassOrChat returns the model's class, defaulting empty to "chat" so callers
// compare against a normalized value (catalog.ClassChat / ClassEmbedding).
func (m Model) ClassOrChat() string {
	if m.Class == "" {
		return catalog.ClassChat
	}
	return m.Class
}

// route is one live instance of a model: the executor for the worker connection
// serving it, tagged with that connection's worker id so the worker's teardown
// removes only its own instances (connection-identified routing — M1 phase 4).
// A model name may have several routes at once: replicas across workers, or a
// reconnecting worker briefly overlapping its old connection. contextWindow is
// the instance's window; replicas of a model are assumed homogeneous, so any
// instance answers metadata.
//
// workerID is the ephemeral per-connection id (the routing and teardown key);
// workerName is the worker's stable operator-supplied --name, which survives
// reconnects. resolve returns the name for usage attribution so a machine's
// ledger totals don't fragment across the many connection ids it cycles through
// over its lifetime (M2 phase 1; docs/follow-ups.md).
// inflight is this instance's live request count, the key least-in-flight
// selection ranks on (ADR-0010): incremented when a request is dispatched to this
// route and decremented once it completes (every path — success, error, cancel).
// It is a pointer so the counter survives a route value being replaced in place on
// re-registration (RegisterInstance), which would otherwise zero a live count.
type route struct {
	workerID      string
	workerName    string
	exec          Executor
	contextWindow int
	class         string // model class (M3 phase 2a); empty = chat
	inflight      *atomic.Int64
}

// localWorkerID tags the in-process models registered at construction (atlas up
// single-node mode). They are never torn down by a worker disconnect, so the id
// is a fixed sentinel rather than a per-connection value.
const localWorkerID = "local"

// Gateway is the client-facing control plane: auth, model resolution, and
// dispatch to a worker. It serves the Anthropic surface: POST /v1/messages
// (buffered and SSE), POST /v1/messages/count_tokens, and GET /v1/models[/{id}].
type Gateway struct {
	auth      Authenticator // validates the API key on every request (ADR-0008)
	createdAt string        // wire created_at stamped on model objects
	logger    *slog.Logger  // one structured line per API request (G10)
	autostart Autostarter   // deploys+waits on a request for an unrouted model (nil = off)
	usage     UsageRecorder // durable per-request usage ledger (phase 6, G13; nil = off)
	metrics   *Metrics      // Prometheus instrumentation (M2 phase 1; nil = off)
	admission *Admission    // per-model load balancing + backpressure (M2 phase 2b; nil = off)
	affinity  *Affinity     // prefix/session-affinity replica selection (M3 phase 1; nil = off)

	// mu guards the route table, which is static in single-node mode but changes
	// as remote workers register and drop their models in fleet mode. route and
	// the model handlers read it under RLock; RegisterInstance/UnregisterWorker
	// mutate it under Lock. Replica selection ranks on each route's own atomic
	// in-flight counter, so it needs only the RLock the read already holds.
	mu      sync.RWMutex
	routes  map[string][]route // canonical name -> live instances (one per worker connection)
	aliases map[string]string  // alias -> canonical name
	order   []string           // canonical names, first-seen order (listing); present iff ≥1 route
}

// NewGateway builds a gateway that authenticates requests with auth, serves each
// model in models by its Name, and resolves each alias to a canonical model
// name. In M0 single-node mode every model maps to one in-process worker;
// operator-defined aliases (e.g. claude-sonnet-4-6 -> a local model) let SDK/tool
// defaults resolve (docs/api-surface.md).
func NewGateway(auth Authenticator, models []Model, aliases map[string]string) *Gateway {
	if aliases == nil {
		aliases = map[string]string{}
	}
	g := &Gateway{
		auth:      auth,
		routes:    make(map[string][]route, len(models)),
		aliases:   aliases,
		order:     make([]string, 0, len(models)),
		createdAt: time.Now().UTC().Format(time.RFC3339),
		logger:    slog.Default(),
	}
	// Initial single-node models never disconnect, so they share a fixed worker id
	// and name (both "local").
	for _, m := range models {
		g.RegisterInstance(localWorkerID, localWorkerID, m)
	}
	return g
}

// SetLogger overrides the gateway's request logger (default slog.Default()).
func (g *Gateway) SetLogger(l *slog.Logger) {
	if l != nil {
		g.logger = l
	}
}

// SetAutostarter attaches the auto-start hook (the scheduler). Call once at
// startup; nil (the default) leaves auto-start off, so an unrouted model is a
// plain 404 — the single-node behavior.
func (g *Gateway) SetAutostarter(a Autostarter) { g.autostart = a }

// SetUsageRecorder attaches the durable usage ledger (phase 6). Call once at
// startup; nil (the default) leaves metering off, so requests serve normally but
// write no usage rows — the behavior in tests and any opt-out deployment.
func (g *Gateway) SetUsageRecorder(u UsageRecorder) { g.usage = u }

// SetMetrics attaches the Prometheus instrumentation (M2 phase 1). Call once at
// startup; nil (the default) leaves metering off, so the gateway serves normally
// but records no series — the behavior in tests and any opt-out deployment.
func (g *Gateway) SetMetrics(m *Metrics) { g.metrics = m }

// SetAdmission attaches the load-balancing + backpressure controller (M2 phase 2b,
// ADR-0010), wiring it to the live replica count and the metrics sink. Call once at
// startup, after SetMetrics; nil (the default) or a controller with PerReplica <= 0
// leaves admission off, so every request is forwarded — M1's behavior.
func (g *Gateway) SetAdmission(a *Admission) {
	g.admission = a
	if a != nil {
		a.replicas = g.routeCount
		a.metrics = g.metrics
	}
}

// SetAffinity attaches prefix/session-affinity routing (M3 phase 1, ADR-0011),
// wiring it to the metrics sink. Call once at startup, after SetMetrics; nil (the
// default) or a controller with a negative tolerance leaves affinity off, so every
// request selects purely by least-in-flight — ADR-0010 behavior.
func (g *Gateway) SetAffinity(a *Affinity) {
	g.affinity = a
	if a != nil {
		a.metrics = g.metrics
	}
}

// canonical maps an alias to its target model name (identity if not an alias),
// so auto-start deploys the served model a client's alias points at.
func (g *Gateway) canonical(name string) string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if c, ok := g.aliases[name]; ok {
		return c
	}
	return name
}

// resolveIntent selects how pickLocked treats a model's replicas, so the inference
// and read-only paths can share one selection routine without a new handler
// silently getting in-flight accounting wrong (the copy-paste hazard the M2 phase-2
// follow-up retired):
//
//   - forDispatch (the inference path, via pick) selects the least-in-flight replica
//     (ADR-0010) and increments that instance's in-flight counter, returning a
//     release the caller invokes once the request completes.
//   - forMetadata (the read-only path, via resolveMeta) returns any live replica
//     without touching the counters, with a no-op release.
type resolveIntent int

const (
	forMetadata resolveIntent = iota
	forDispatch
)

// noopRelease is the release returned when nothing was counted (forMetadata, or a
// failed resolve), so callers can defer it unconditionally.
var noopRelease = func() {}

// dispatchPrep resolves, admits, and selects a replica for an inference request —
// the single entry point for the dispatch surfaces (POST /v1/messages and the
// OpenAI mirror). It (1) ensures the model is servable, auto-starting an unrouted
// catalog model; (2) acquires an admission slot, blocking in the bounded queue and
// shedding a retryable 429/529 beyond capacity (ADR-0010); (3) selects a replica,
// preferring the request's affine replica when affinity is on and it is within load
// tolerance, otherwise least-in-flight (ADR-0011). affinityKey is the request's
// routing key ("" when affinity is disabled or no key derives). On success it
// returns the model, the serving worker's stable name, and a release that frees both
// the admission slot and the replica's in-flight slot. On failure it returns the
// *anthropic.Error the caller should render (404 unknown model, 429 momentarily full,
// 529 overloaded) and a nil release.
func (g *Gateway) dispatchPrep(ctx context.Context, name, affinityKey, wantClass string) (Model, string, func(), *anthropic.Error) {
	canon, ok := g.ensure(ctx, name)
	if !ok {
		return Model{}, "", nil, modelNotFoundErr(name)
	}
	// Class routing (M3 phase 2a, ADR-0012): the model must serve the class this
	// endpoint dispatches (chat for /v1/messages and the OpenAI chat mirror,
	// embedding for /v1/embeddings). A mismatch — e.g. an embeddings call against a
	// chat model — is a clean client error, rejected before an admission slot is
	// taken, never a 5xx.
	if cls, known := g.routeClass(canon); known && cls != wantClass {
		return Model{}, "", nil, wrongClassErr(canon, wantClass)
	}
	admitRelease, apiErr := g.admission.Acquire(ctx, canon)
	if apiErr != nil {
		return Model{}, "", nil, apiErr
	}
	model, workerName, pickRelease, ok := g.pick(canon, affinityKey)
	if !ok {
		// The model was servable a moment ago but no replica is live now (it dropped
		// between admission and selection): overloaded, retryable. Count it like every
		// other shed so the shed metric doesn't under-report overload.
		admitRelease()
		g.metrics.incShed(canon, "529")
		return Model{}, "", nil, overloadedErr(g.admission.retryAfterSecs())
	}
	return model, workerName, func() { pickRelease(); admitRelease() }, nil
}

// ensure makes a model servable for an inference request: it resolves an alias to
// the canonical served name and, if no replica is live, auto-starts one and blocks
// until ready (M1 phase 4b-2). On a live model it records activity (Touch) so a
// steadily used auto-started model stays warm. ok is false when the model cannot be
// served (unknown, auto-start disabled, or the wait timed out). It selects no
// replica and counts no in-flight — pick does that, after admission.
func (g *Gateway) ensure(ctx context.Context, name string) (string, bool) {
	canon := g.canonical(name)
	if g.routeCount(canon) > 0 {
		if g.autostart != nil {
			g.autostart.Touch(canon)
		}
		return canon, true
	}
	if g.autostart == nil {
		return "", false
	}
	if !g.autostart.EnsureModel(ctx, canon) {
		return "", false
	}
	return canon, true
}

// pick selects a live replica of a canonical model name — the affine replica when
// affinity is on and within load tolerance, otherwise least-in-flight (ADR-0011) —
// increments that instance's in-flight counter, and returns a single-shot release
// that decrements it. affinityKey is the request's routing key ("" disables
// affinity for this pick). ok is false if no replica is live (e.g. the only one
// dropped between admission and selection).
func (g *Gateway) pick(canon, affinityKey string) (Model, string, func(), bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.pickLocked(canon, forDispatch, affinityKey)
}

// resolveMeta resolves a model name (alias or canonical) for a read-only request
// (model listing, count_tokens): any live replica answers, with no auto-start and
// no in-flight accounting. ok is false when no replica is live.
func (g *Gateway) resolveMeta(name string) (Model, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	m, _, _, ok := g.pickLocked(name, forMetadata, "")
	return m, ok
}

// routeCount is the live replica count for a canonical model name — the admission
// controller's capacity input (capacity = per-replica concurrency × this).
func (g *Gateway) routeCount(canon string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.routes[canon])
}

// routeClass returns the model class of a canonical model's live replicas,
// normalized so empty reads as chat (M3 phase 2a). known is false when no replica
// is live (so the caller falls through to the usual not-found / overload paths
// rather than rejecting on a class it cannot determine). Replicas of one model are
// homogeneous, so the first answers.
func (g *Gateway) routeClass(canon string) (class string, known bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rs := g.routes[canon]
	if len(rs) == 0 {
		return "", false
	}
	if rs[0].class == "" {
		return catalog.ClassChat, true
	}
	return rs[0].class, true
}

// pickLocked selects a live instance of name (resolving an alias first), for a
// caller already holding mu. For forDispatch it selects a replica — the affinityKey's
// affine replica when affinity is on and within load tolerance, otherwise
// least-in-flight (ADR-0010/0011) — increments that instance's counter, and returns a
// single-shot release that decrements it; for forMetadata it returns any live
// instance (replicas are homogeneous, so any answers metadata) without touching the
// counters and ignores affinityKey. The second result is the chosen instance's stable
// worker name (not its ephemeral connection id), so usage attribution survives the
// worker's reconnects (M2 phase 1).
func (g *Gateway) pickLocked(name string, intent resolveIntent, affinityKey string) (Model, string, func(), bool) {
	if canon, ok := g.aliases[name]; ok {
		name = canon
	}
	rs := g.routes[name]
	if len(rs) == 0 {
		return Model{}, "", noopRelease, false
	}
	r := rs[0]
	release := noopRelease
	if intent == forDispatch {
		r = g.affinity.selectReplica(name, affinityKey, rs)
		r.inflight.Add(1)
		release = releaseOnce(r.inflight)
	}
	return Model{Name: name, Exec: r.exec, ContextWindow: r.contextWindow, Class: r.class}, r.workerName, release, true
}

// leastInFlight returns the route with the fewest live in-flight requests, ties
// broken uniformly at random (reservoir sampling over the running minimum). rs
// must be non-empty. Each counter is read once; a concurrent change between reads
// is acceptable — selection is best-effort load spreading and self-corrects on the
// next request (ADR-0010).
func leastInFlight(rs []route) route {
	best := rs[0]
	bestN := best.inflight.Load()
	ties := 1
	for i := 1; i < len(rs); i++ {
		switch n := rs[i].inflight.Load(); {
		case n < bestN:
			best, bestN, ties = rs[i], n, 1
		case n == bestN:
			ties++
			if mrand.IntN(ties) == 0 {
				best = rs[i]
			}
		}
	}
	return best
}

// releaseOnce returns a function that decrements c exactly once however many times
// it is called, so a caller can defer it without double-counting on overlapping
// completion paths (e.g. an error return that also runs a deferred cleanup).
func releaseOnce(c *atomic.Int64) func() {
	var once sync.Once
	return func() { once.Do(func() { c.Add(-1) }) }
}

// RegisterInstance adds one live instance of a model, owned by the given worker
// connection. The hub calls it per served model when a worker joins, pointing
// Exec at that connection's remote executor; the gateway then serves the model
// exactly as an in-process one. A model name may hold several instances at once
// — replicas across workers, or a reconnecting worker briefly overlapping its
// old connection — and a request resolves to the name as long as any instance
// is live. Routes are connection-identified so one worker's teardown
// (UnregisterWorker) removes only its own instances, never a route a different
// live connection installed (M1 phase 4).
//
// workerID is the ephemeral connection id (the routing/teardown key); workerName
// is the worker's stable --name, recorded on the route so usage attribution can
// group by a name that survives reconnects (M2 phase 1).
func (g *Gateway) RegisterInstance(workerID, workerName string, m Model) {
	g.mu.Lock()
	rs := g.routes[m.Name]
	next := route{workerID: workerID, workerName: workerName, exec: m.Exec, contextWindow: m.ContextWindow, class: m.Class, inflight: new(atomic.Int64)}
	// One route per (worker connection, model): a re-registration of the same pair
	// replaces in place rather than appending a duplicate — e.g. a worker re-emits
	// model_ready for a model it already serves. Duplicates would double-weight
	// that connection in replica selection and inflate replica counts, and
	// UnregisterWorker removes all of a worker's routes at once, so they would only
	// self-heal on disconnect.
	for i, r := range rs {
		if r.workerID == workerID {
			// Preserve the live in-flight counter across the re-emit: a request may be
			// in flight on this instance right now, and a fresh counter would lose it.
			next.inflight = r.inflight
			rs[i] = next
			g.mu.Unlock()
			return // in-place replace: capacity unchanged, nothing to promote
		}
	}
	if len(rs) == 0 {
		g.order = append(g.order, m.Name)
	}
	g.routes[m.Name] = append(rs, next)
	g.mu.Unlock()

	// A new replica raised this model's admission capacity. Promote any waiters
	// blocked in the queue now, rather than making them wait out MaxWait for a slot
	// that already exists — done after releasing g.mu, since promotion re-reads the
	// replica count (lock order a.mu → g.mu). Safe when admission is nil/disabled.
	g.admission.promoteForCapacity(m.Name)
}

// UnregisterInstance removes one worker's instance of a single model, called
// when the scheduler unloads that model from that worker (M1 phase 4b) — unlike
// UnregisterWorker, the worker's other models stay served. A model whose last
// instance is removed stops resolving and drops out of the listing. Unknown
// (worker, model) pairs are a no-op.
func (g *Gateway) UnregisterInstance(workerID, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if canon, ok := g.aliases[name]; ok {
		name = canon
	}
	rs, ok := g.routes[name]
	if !ok {
		return
	}
	kept := make([]route, 0, len(rs))
	for _, r := range rs {
		if r.workerID != workerID {
			kept = append(kept, r)
		}
	}
	if len(kept) == len(rs) {
		return
	}
	if len(kept) == 0 {
		delete(g.routes, name)
		g.removeFromOrder(name)
		return
	}
	g.routes[name] = kept
}

// UnregisterWorker removes every instance owned by a worker connection, called
// when that worker drains or disconnects. A model whose last instance is removed
// stops resolving and drops out of the listing; a model still served by another
// connection keeps its remaining instances. Unknown worker ids are a no-op.
func (g *Gateway) UnregisterWorker(workerID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for name, rs := range g.routes {
		kept := make([]route, 0, len(rs))
		for _, r := range rs {
			if r.workerID != workerID {
				kept = append(kept, r)
			}
		}
		switch {
		case len(kept) == 0:
			delete(g.routes, name)
			g.removeFromOrder(name)
		case len(kept) != len(rs):
			g.routes[name] = kept
		}
	}
}

// removeFromOrder drops name from the listing order. The caller holds mu.
func (g *Gateway) removeFromOrder(name string) {
	for i, n := range g.order {
		if n == name {
			g.order = append(g.order[:i], g.order[i+1:]...)
			return
		}
	}
}

// Handler returns the gateway's HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", g.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", g.handleCountTokens)
	mux.HandleFunc("POST /v1/chat/completions", g.handleChatCompletions)
	mux.HandleFunc("POST /v1/embeddings", g.handleEmbeddings)
	mux.HandleFunc("POST /v1/rerank", g.handleRerank)
	mux.HandleFunc("GET /v1/models", g.handleListModels)
	mux.HandleFunc("GET /v1/models/{id}", g.handleGetModel)
	// Liveness: the process is up and serving. Says nothing about whether a
	// model can answer — use /readyz for that.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		anthropic.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: a model is servable. In M0 single-node mode a model is
	// registered only after its worker reports healthy (worker.Start blocks on
	// the engine's /health), so "has a registered model" is exactly "a model is
	// servable". 503 until then, so an orchestrator can gate traffic on it.
	mux.HandleFunc("GET /readyz", g.handleReadyz)
	return g.withRequestLog(mux)
}

// handleReadyz reports readiness: 200 once at least one model is servable,
// 503 otherwise (G10, criterion 8).
func (g *Gateway) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	g.mu.RLock()
	n := len(g.routes)
	g.mu.RUnlock()
	if n == 0 {
		anthropic.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no models servable"})
		return
	}
	anthropic.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// maxRequestBytes caps a request body. Generous for chat; a hard ceiling so a
// malformed length header can't exhaust memory.
const maxRequestBytes = 32 << 20 // 32 MiB

func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	id, authErr := g.authenticate(r)
	if authErr != nil {
		anthropic.WriteError(w, authErr)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("could not read request body"))
		return
	}

	var req anthropic.MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("request body is not valid JSON"))
		return
	}

	coreReq, err := req.ToCore()
	if err != nil {
		writeErr(w, err)
		return
	}

	if !g.modelPermitted(id, coreReq.Model) {
		anthropic.WriteError(w, forbiddenModelErr(coreReq.Model))
		return
	}

	affinityKey := g.affinity.routingKey(r, coreReq)
	model, workerName, release, apiErr := g.dispatchPrep(r.Context(), coreReq.Model, affinityKey, catalog.ClassChat)
	if apiErr != nil {
		anthropic.WriteError(w, apiErr)
		return
	}
	// Hold the admission slot and the instance's in-flight slot until the request
	// completes — every return below runs this, so a failed assertion, an Execute
	// error, and a finished stream all free both.
	defer release()

	// Assert the prompt fits the model's window before dispatch, so an oversized
	// request fails with a clean 400 rather than a garbled engine overflow
	// (docs/m0-acceptance.md context-window handling). The returned count is the
	// prompt's input tokens, reused to attribute input usage on an interrupted
	// stream (the engine reports usage only on a clean completion).
	promptTokens, err := g.assertContextFits(r.Context(), model, coreReq)
	if err != nil {
		writeErr(w, err)
		return
	}

	// The gateway owns stop-sequence semantics, so the engine never sees them
	// and behavior is identical across engines.
	stops := coreReq.StopSequences
	coreReq.StopSequences = nil

	// The client may have addressed an alias; dispatch under the canonical served
	// name (a remote worker routes by req.Model) but echo the requested name back.
	requested := coreReq.Model
	dispatch := coreReq
	dispatch.Model = model.Name

	tags := usageTags{keyID: id.KeyID, workerID: workerName, model: model.Name, inputTokens: promptTokens}

	if req.Stream {
		g.streamMessages(w, r, model.Exec, dispatch, requested, stops, tags)
		return
	}

	resp, err := model.Exec.Execute(r.Context(), dispatch)
	if err != nil {
		writeErr(w, err)
		return
	}

	resp, seq := core.ApplyStopSequences(resp, stops)
	var stopSeq *string
	if seq != "" {
		stopSeq = &seq
	}

	recordBillableUsage(r.Context(), tags, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	anthropic.WriteJSON(w, http.StatusOK, anthropic.FromCore(newMessageID(), requested, resp, stopSeq))
}

// assertContextFits rejects a request whose prompt plus max_tokens cannot fit
// the model's context window. The window is the engine's real n_ctx; the count
// is the engine's real tokenizer. A max_tokens that alone meets the window is
// always too big and is caught without an engine round-trip. Counting is
// best-effort: if the tokenizer call fails (and the engine is truly down,
// dispatch surfaces that as a 529), the request proceeds rather than being
// blocked on a transient hiccup.
//
// It returns the counted prompt tokens (0 when not counted: unknown window,
// non-counting engine, or a failed count) so the caller can attribute input
// usage on an interrupted stream, where the engine never reports its own count.
func (g *Gateway) assertContextFits(ctx context.Context, model Model, req core.Request) (int, error) {
	if model.ContextWindow <= 0 {
		return 0, nil // unknown window: nothing to assert against
	}
	if req.MaxTokens >= model.ContextWindow {
		return 0, anthropic.ErrInvalid("max_tokens (%d) exceeds the model's %d-token context window", req.MaxTokens, model.ContextWindow)
	}
	tc, ok := model.Exec.(TokenCounter)
	if !ok {
		return 0, nil
	}
	// Route to the executor under the canonical served name: a remote worker
	// dispatches by req.Model, and the client may have addressed an alias.
	req.Model = model.Name
	n, err := tc.CountTokens(ctx, req)
	if err != nil {
		return 0, nil // best-effort; see doc comment
	}
	if n+req.MaxTokens > model.ContextWindow {
		return n, anthropic.ErrInvalid(
			"prompt is too long: %d input tokens + %d max_tokens exceeds the model's %d-token context window",
			n, req.MaxTokens, model.ContextWindow)
	}
	return n, nil
}

// handleCountTokens serves POST /v1/messages/count_tokens: the prompt's token
// count from the target model's real tokenizer (criterion 5).
func (g *Gateway) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	id, authErr := g.authenticate(r)
	if authErr != nil {
		anthropic.WriteError(w, authErr)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("could not read request body"))
		return
	}

	var req anthropic.CountTokensRequest
	if err := json.Unmarshal(body, &req); err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("request body is not valid JSON"))
		return
	}

	coreReq, err := req.ToCore()
	if err != nil {
		writeErr(w, err)
		return
	}

	if !g.modelPermitted(id, coreReq.Model) {
		anthropic.WriteError(w, forbiddenModelErr(coreReq.Model))
		return
	}

	model, ok := g.resolveMeta(coreReq.Model)
	if !ok {
		writeModelNotFound(w, coreReq.Model)
		return
	}

	tc, ok := model.Exec.(TokenCounter)
	if !ok {
		anthropic.WriteError(w, &anthropic.Error{Status: http.StatusInternalServerError, Type: anthropic.ErrAPI, Msg: "count_tokens unsupported for this model"})
		return
	}
	// Count under the canonical served name (a remote worker routes by req.Model)
	// even when the client addressed an alias.
	countReq := coreReq
	countReq.Model = model.Name
	n, err := tc.CountTokens(r.Context(), countReq)
	if err != nil {
		writeErr(w, err)
		return
	}

	recordUsage(r.Context(), coreReq.Model, n, 0)
	anthropic.WriteJSON(w, http.StatusOK, anthropic.CountTokensResponse{InputTokens: n})
}

// handleListModels serves GET /v1/models: every deployed model followed by
// every alias, each with context-window metadata (criterion 4).
func (g *Gateway) handleListModels(w http.ResponseWriter, r *http.Request) {
	if _, authErr := g.authenticate(r); authErr != nil {
		anthropic.WriteError(w, authErr)
		return
	}

	g.mu.RLock()
	infos := make([]anthropic.ModelInfo, 0, len(g.order)+len(g.aliases))
	for _, name := range g.order {
		infos = append(infos, g.modelInfoLocked(name))
	}
	aliasNames := make([]string, 0, len(g.aliases))
	for a := range g.aliases {
		aliasNames = append(aliasNames, a)
	}
	sort.Strings(aliasNames)
	for _, a := range aliasNames {
		infos = append(infos, g.modelInfoLocked(a))
	}
	g.mu.RUnlock()

	anthropic.WriteJSON(w, http.StatusOK, anthropic.NewModelList(infos))
}

// handleGetModel serves GET /v1/models/{id} for an alias or a canonical name.
func (g *Gateway) handleGetModel(w http.ResponseWriter, r *http.Request) {
	if _, authErr := g.authenticate(r); authErr != nil {
		anthropic.WriteError(w, authErr)
		return
	}

	id := r.PathValue("id")
	if _, ok := g.resolveMeta(id); !ok {
		writeModelNotFound(w, id)
		return
	}
	anthropic.WriteJSON(w, http.StatusOK, g.modelInfo(id))
}

// modelInfo builds the wire model object for an alias or canonical id. The id
// echoes what the client addressed; display_name is the canonical model it
// resolves to, and context_window is that model's window. The caller must have
// confirmed id resolves.
func (g *Gateway) modelInfo(id string) anthropic.ModelInfo {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.modelInfoLocked(id)
}

// modelInfoLocked is modelInfo without locking, for callers already holding mu.
func (g *Gateway) modelInfoLocked(id string) anthropic.ModelInfo {
	model, _, _, _ := g.pickLocked(id, forMetadata, "")
	return anthropic.NewModelInfo(id, model.Name, g.createdAt, model.ContextWindow)
}

// streamMessages serves a streaming POST /v1/messages. It opens the SSE
// response, drives the executor (native streaming if supported, else a
// buffered Execute replayed as one stream), and applies stop sequences as text
// arrives so behavior matches the non-streaming path.
//
// req is the request to dispatch (its Model is the canonical served name);
// echoModel is the name the client addressed (possibly an alias), echoed back
// on the stream's message. Once the SSE headers are written the status is
// committed: a mid-stream engine failure becomes an error event, not an HTTP
// error.
func (g *Gateway) streamMessages(w http.ResponseWriter, r *http.Request, exec Executor, req core.Request, echoModel string, stops []string, tags usageTags) {
	sw, err := anthropic.NewStreamWriter(w, newMessageID(), echoModel)
	if err != nil {
		anthropic.WriteError(w, &anthropic.Error{Status: http.StatusInternalServerError, Type: anthropic.ErrAPI, Msg: "streaming unsupported"})
		return
	}
	if err := sw.Start(0); err != nil {
		return // client went away; nothing more we can do
	}

	sink := &streamSink{sw: sw, scanner: core.NewStopSequenceScanner(stops), reason: core.StopEndTurn}

	if streamer, ok := exec.(StreamExecutor); ok {
		err = streamer.ExecuteStream(r.Context(), req, sink)
	} else {
		err = bufferedStream(r.Context(), exec, req, sink)
	}
	if err != nil {
		// Interrupted mid-stream (worker drop or client-write failure): record the
		// usage emitted up to the cut rather than zero, so the ledger is not
		// systematically short on interrupted streams (G13 interrupted case). A
		// stream that produced nothing records nothing, matching the non-streaming
		// error path.
		if out := sink.partialOutputTokens(); out > 0 {
			recordBillableUsage(r.Context(), tags, partialInputTokens(sink.usage.InputTokens, tags), out)
		}
		_ = sw.Error(anthropic.ErrAPI, "engine error during generation")
		return
	}

	recordBillableUsage(r.Context(), tags, sink.usage.InputTokens, sink.usage.OutputTokens)
	_ = sw.Finish(sink.reason, sink.stopSeq, sink.usage)
}

// bufferedStream adapts a non-streaming Executor to the streaming sink by
// running Execute and replaying each content block as the matching sink events,
// then Done. It preserves block order so a text-then-tool_use response streams
// the same shape a native streamer would produce.
func bufferedStream(ctx context.Context, exec Executor, req core.Request, sink core.StreamSink) error {
	resp, err := exec.Execute(ctx, req)
	if err != nil {
		return err
	}
	for i, b := range resp.Blocks {
		switch b.Type {
		case core.BlockThinking:
			if b.Thinking == "" {
				continue
			}
			if err := sink.Thinking(b.Thinking); err != nil {
				return err
			}
		case core.BlockText:
			if b.Text == "" {
				continue
			}
			if err := sink.Text(b.Text); err != nil {
				if errors.Is(err, core.ErrStopStreaming) {
					return nil
				}
				return err
			}
		case core.BlockToolUse:
			if err := sink.ToolCallStart(i, b.ID, b.Name); err != nil {
				return err
			}
			if err := sink.ToolCallDelta(i, string(b.Input)); err != nil {
				return err
			}
		}
	}
	return sink.Done(resp.StopReason, resp.Usage)
}

// streamSink interposes between an engine's deltas and the SSE writer: it runs
// text through the stop-sequence scanner (truncating and ending the stream when
// one matches) and records the final stop reason, sequence, and usage for the
// closing message_delta.
type streamSink struct {
	sw       *anthropic.StreamWriter
	scanner  *core.StopSequenceScanner
	reason   core.StopReason
	stopSeq  *string
	usage    core.Usage
	outBytes int // emitted output text bytes, for the interrupted-stream estimate
}

// partialOutputTokens returns the output-token count to record when a stream is
// interrupted before Done. If the engine already reported usage (it never does
// mid-stream today, but be safe) that exact count wins; otherwise it estimates
// from the bytes emitted so far, since no per-delta token count is available
// (the engine reports usage only in its final chunk). The estimate keeps the
// ledger from recording zero for a stream that did produce output (G13).
func (s *streamSink) partialOutputTokens() int {
	if s.usage.OutputTokens > 0 {
		return s.usage.OutputTokens
	}
	return estimateTokens(s.outBytes)
}

// partialInputTokens returns the input-token count to record for an interrupted
// stream. The engine reports its own input count only on a clean completion
// (Done), so on an interruption engineInput is 0; fall back to the prompt count
// computed before dispatch (assertContextFits, carried on tags) so the ledger
// records the input the request actually consumed rather than zero. Shared by
// the Anthropic and OpenAI stream paths.
func partialInputTokens(engineInput int, tags usageTags) int {
	if engineInput > 0 {
		return engineInput
	}
	return tags.inputTokens
}

// Thinking forwards a reasoning delta straight to the writer. Stop sequences
// match the model's visible answer, not its reasoning, so thinking text bypasses
// the scanner (and reasoning always precedes text, so the scanner is still empty
// here anyway).
func (s *streamSink) Thinking(delta string) error {
	return s.sw.ThinkingDelta(delta)
}

func (s *streamSink) Text(delta string) error {
	emit, matched := s.scanner.Push(delta)
	if emit != "" {
		if err := s.sw.TextDelta(emit); err != nil {
			return err
		}
		s.outBytes += len(emit)
	}
	if matched {
		s.reason = core.StopStopSequence
		seq := s.scanner.Matched()
		s.stopSeq = &seq
		return core.ErrStopStreaming
	}
	return nil
}

// ToolCallStart opens a tool_use block. Stop sequences apply to model text, not
// tool arguments, so any text held back by the scanner is flushed first to keep
// it ahead of the tool block on the wire.
func (s *streamSink) ToolCallStart(_ int, id, name string) error {
	if tail := s.scanner.Flush(); tail != "" {
		if err := s.sw.TextDelta(tail); err != nil {
			return err
		}
	}
	s.reason = core.StopToolUse
	return s.sw.ToolUseStart(id, name)
}

func (s *streamSink) ToolCallDelta(_ int, argsFragment string) error {
	return s.sw.ToolUseDelta(argsFragment)
}

func (s *streamSink) Done(reason core.StopReason, usage core.Usage) error {
	if tail := s.scanner.Flush(); tail != "" {
		if err := s.sw.TextDelta(tail); err != nil {
			return err
		}
	}
	s.reason = reason
	s.usage = usage
	return nil
}

// authenticate validates a request's API key against the store, returning the
// caller's Identity. A nil error means authenticated; otherwise the returned
// *anthropic.Error is ready to write (401 for a missing/unknown/revoked key, 500
// for an auth-backend failure). The OpenAI handler reshapes the same error onto
// its envelope via writeOpenAIErr.
func (g *Gateway) authenticate(r *http.Request) (Identity, *anthropic.Error) {
	secret := apiKeyFromRequest(r)
	if secret == "" {
		return Identity{}, unauthorizedErr()
	}
	id, ok, err := g.auth.Authenticate(r.Context(), secret)
	if err != nil {
		return Identity{}, &anthropic.Error{Status: http.StatusInternalServerError, Type: anthropic.ErrAPI, Msg: "authentication backend error"}
	}
	if !ok {
		return Identity{}, unauthorizedErr()
	}
	return id, nil
}

// RequireAdmin wraps an admin handler so only a request carrying a valid
// admin-scoped API key reaches it: a missing/unknown/revoked key is 401, a valid
// but non-admin key is 403, an auth-backend failure is 500. The /admin/* control
// surface (worker drain, deploy/scale/stop) uses it; it shares the same
// Authenticator and key store as the client gateway (ADR-0008, phase 5b), so one
// key system covers both surfaces — scope on the key, not a second secret.
func RequireAdmin(auth Authenticator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := apiKeyFromRequest(r)
		if secret == "" {
			writeAdminError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		id, ok, err := auth.Authenticate(r.Context(), secret)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "authentication backend error")
			return
		}
		if !ok {
			writeAdminError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		if !id.Admin {
			writeAdminError(w, http.StatusForbidden, "this API key is not permitted to use the admin surface")
			return
		}
		next(w, r)
	}
}

// writeAdminError renders an admin-surface error as a small JSON object. The
// admin surface is Atlas's own control plane (the CLI keys on status codes), not
// an Anthropic-compatible surface, so it does not use the Anthropic envelope.
func writeAdminError(w http.ResponseWriter, status int, msg string) {
	anthropic.WriteJSON(w, status, map[string]string{"error": msg})
}

// apiKeyFromRequest extracts the presented secret from x-api-key or
// Authorization: Bearer (clients vary — docs/api-surface.md).
func apiKeyFromRequest(r *http.Request) string {
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	if after, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
		return after
	}
	return ""
}

// modelPermitted reports whether id may use the requested model. An empty
// allowlist permits every model; otherwise the requested name or its canonical
// target (the client may have addressed an alias) must be listed.
func (g *Gateway) modelPermitted(id Identity, requested string) bool {
	if len(id.Allowlist) == 0 {
		return true
	}
	canon := g.canonical(requested)
	for _, allowed := range id.Allowlist {
		if allowed == requested || allowed == canon {
			return true
		}
	}
	return false
}

func unauthorizedErr() *anthropic.Error {
	return &anthropic.Error{Status: http.StatusUnauthorized, Type: anthropic.ErrAuthentication, Msg: "missing or invalid API key"}
}

func forbiddenModelErr(model string) *anthropic.Error {
	return &anthropic.Error{Status: http.StatusForbidden, Type: anthropic.ErrPermission, Msg: "this API key is not permitted to use model: " + model}
}

// readBody reads a request body under the size cap.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
}

// modelNotFoundErr is the 404 for an unknown or undeployed model — distinct from
// the retryable 429/529 a known-but-saturated model sheds (ADR-0010).
func modelNotFoundErr(model string) *anthropic.Error {
	return &anthropic.Error{
		Status: http.StatusNotFound,
		Type:   anthropic.ErrNotFound,
		Msg:    "model not found: " + model,
	}
}

func writeModelNotFound(w http.ResponseWriter, model string) {
	anthropic.WriteError(w, modelNotFoundErr(model))
}

// wrongClassErr is the clean 400 for a request sent to a model of the wrong class
// (M3 phase 2a, ADR-0012): e.g. an embeddings call against a chat model, or a chat
// call against an embedding model. The model exists, so it is not a 404; it simply
// does not serve this endpoint's class.
func wrongClassErr(model, wantClass string) *anthropic.Error {
	return anthropic.ErrInvalid("model %q does not serve %s requests", model, wantClass)
}

// rateLimitedErr is the retryable 429: the model has live capacity but is
// momentarily full (admission queue full, or the max wait elapsed). retryAfter is
// the advertised Retry-After in seconds.
func rateLimitedErr(retryAfter int) *anthropic.Error {
	return &anthropic.Error{
		Status:     http.StatusTooManyRequests,
		Type:       anthropic.ErrRateLimit,
		Msg:        "the model is momentarily at capacity; retry shortly",
		RetryAfter: retryAfter,
	}
}

// overloadedErr is the retryable 529: no live replica can serve the model right now
// (none placed, or the only one dropped under load).
func overloadedErr(retryAfter int) *anthropic.Error {
	return &anthropic.Error{
		Status:     statusOverloaded,
		Type:       anthropic.ErrOverloaded,
		Msg:        "the model is overloaded; retry shortly",
		RetryAfter: retryAfter,
	}
}

// writeErr renders an error as its Anthropic envelope. An *anthropic.Error
// carries its own status; an engine-unavailable failure (core.ErrEngineUnavailable)
// becomes a retryable 529 overloaded_error; anything else is a 500 api_error
// (internal failures the client shouldn't see details of).
func writeErr(w http.ResponseWriter, err error) {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		anthropic.WriteError(w, apiErr)
		return
	}
	if errors.Is(err, core.ErrEngineUnavailable) {
		anthropic.WriteError(w, &anthropic.Error{
			Status: statusOverloaded,
			Type:   anthropic.ErrOverloaded,
			Msg:    "the inference engine is unavailable; retry shortly",
		})
		return
	}
	anthropic.WriteError(w, &anthropic.Error{
		Status: http.StatusInternalServerError,
		Type:   anthropic.ErrAPI,
		Msg:    "internal error",
	})
}

func newMessageID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// estimateTokens approximates a token count from a byte length, used only for
// the output of an interrupted stream where no exact count is available (the
// engine reports usage only at end of stream). ~4 bytes/token is the standard
// rough heuristic; any non-empty output rounds up to at least one token so the
// ledger never records zero for a stream that produced text.
func estimateTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	if t := bytes / 4; t > 0 {
		return t
	}
	return 1
}
