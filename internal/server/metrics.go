package server

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Metrics is the control plane's Prometheus instrumentation (M2 phase 1). It owns
// its own registry — so /metrics carries only Atlas series, not Go-runtime noise —
// and the collectors the gateway feeds from the request path: request rate and
// status, request latency, per-model/per-worker token counters, and an in-flight
// gauge. Connected-worker count is a GaugeFunc read live from the hub at scrape
// time, so the hub needs no metrics plumbing.
//
// The same registry backs the JSON snapshot `atlas status` renders (Snapshot),
// so the CLI inspection tool and a Prometheus scrape report identical numbers —
// one data path, two presentations (build plan decision 2). The admission
// queue-depth gauge and shed counters (M2 phase 2b) are fed by the backpressure
// layer (admission.go) alongside the logic that moves them.
//
// All methods are safe on a nil *Metrics (metering disabled), so the gateway can
// call them unconditionally.
type Metrics struct {
	reg *prometheus.Registry

	requests        *prometheus.CounterVec   // by path, status
	requestDuration *prometheus.HistogramVec // by path
	inputTokens     *prometheus.CounterVec   // by model, worker
	outputTokens    *prometheus.CounterVec   // by model, worker
	inFlight        prometheus.Gauge
	queueDepth      *prometheus.GaugeVec   // by model — admission queue (M2 phase 2b)
	shed            *prometheus.CounterVec // by model, code (429/529) — backpressure sheds
	affinity        *prometheus.CounterVec // by model, result (hit/miss) — affinity routing (M3 phase 1)
	warmKeys        *prometheus.GaugeVec   // by model, worker — distinct warm affinity keys per replica

	// workerSource yields the current connected-worker count for the GaugeFunc.
	// Stored as an atomic so the hub-backed source can be wired after construction
	// (SetWorkerCountSource); nil until then, reported as 0.
	workerSource atomic.Pointer[func() int]
}

// Metric names. Exported as constants so tests and the status snapshot reference
// the same strings the collectors register under.
const (
	metricRequests        = "atlas_requests_total"
	metricRequestDuration = "atlas_request_duration_seconds"
	metricInputTokens     = "atlas_input_tokens_total"
	metricOutputTokens    = "atlas_output_tokens_total"
	metricInFlight        = "atlas_requests_in_flight"
	metricWorkers         = "atlas_connected_workers"
	metricQueueDepth      = "atlas_queue_depth"
	metricShed            = "atlas_shed_total"
	metricAffinity        = "atlas_affinity_total"
	metricWarmKeys        = "atlas_affinity_warm_keys"
)

// NewMetrics builds the collectors and registers them on a private registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricRequests,
			Help: "Total API requests by route and HTTP status.",
		}, []string{"path", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricRequestDuration,
			Help:    "API request duration in seconds by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"path"}),
		inputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricInputTokens,
			Help: "Total input (prompt) tokens billed, by model and serving worker.",
		}, []string{"model", "worker"}),
		outputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricOutputTokens,
			Help: "Total output (completion) tokens billed, by model and serving worker.",
		}, []string{"model", "worker"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: metricInFlight,
			Help: "API requests currently being served.",
		}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricQueueDepth,
			Help: "Requests currently waiting in a model's admission queue.",
		}, []string{"model"}),
		shed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricShed,
			Help: "Total requests shed by backpressure, by model and HTTP status code (429/529).",
		}, []string{"model", "code"}),
		affinity: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricAffinity,
			Help: "Total affinity-routing decisions by model and result (hit = served by the affine replica, miss = fell back to least-in-flight under load).",
		}, []string{"model", "result"}),
		warmKeys: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricWarmKeys,
			Help: "Distinct recently-warmed affinity keys routed to each replica, by model and serving worker.",
		}, []string{"model", "worker"}),
	}
	workers := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: metricWorkers,
		Help: "Workers currently connected to the hub.",
	}, func() float64 {
		if f := m.workerSource.Load(); f != nil {
			return float64((*f)())
		}
		return 0
	})
	m.reg.MustRegister(m.requests, m.requestDuration, m.inputTokens, m.outputTokens, m.inFlight, m.queueDepth, m.shed, m.affinity, m.warmKeys, workers)
	return m
}

// SetWorkerCountSource wires the live connected-worker count (e.g. the hub's
// worker count) behind the atlas_connected_workers gauge. Call once at startup.
func (m *Metrics) SetWorkerCountSource(f func() int) {
	if m != nil {
		m.workerSource.Store(&f)
	}
}

// Handler serves the Prometheus exposition format for the metrics registry. It is
// mounted admin-scoped (build plan decision 1).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// observeRequest records one completed request's route, status, and duration.
func (m *Metrics) observeRequest(path string, status int, dur time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(path, strconv.Itoa(status)).Inc()
	m.requestDuration.WithLabelValues(path).Observe(dur.Seconds())
}

// addTokens adds a billable request's token counts to the per-model/worker
// counters. Gated by the caller on billable requests, so these series track the
// same usage the ledger records.
func (m *Metrics) addTokens(model, worker string, in, out int) {
	if m == nil {
		return
	}
	if in > 0 {
		m.inputTokens.WithLabelValues(model, worker).Add(float64(in))
	}
	if out > 0 {
		m.outputTokens.WithLabelValues(model, worker).Add(float64(out))
	}
}

// setQueueDepth records a model's current admission-queue depth (M2 phase 2b).
func (m *Metrics) setQueueDepth(model string, depth int) {
	if m != nil {
		m.queueDepth.WithLabelValues(model).Set(float64(depth))
	}
}

// incShed counts one backpressure shed for a model under an HTTP status code
// ("429" momentarily full, "529" overloaded).
func (m *Metrics) incShed(model, code string) {
	if m != nil {
		m.shed.WithLabelValues(model, code).Inc()
	}
}

// incAffinity counts one affinity-routing decision for a model: a hit (the affine
// replica was within load tolerance and chosen) or a miss (it was past tolerance, so
// selection fell back to least-in-flight).
func (m *Metrics) incAffinity(model string, hit bool) {
	if m != nil {
		m.affinity.WithLabelValues(model, affinityResult(hit)).Inc()
	}
}

// affinityResult is the "result" label value for an affinity decision.
func affinityResult(hit bool) string {
	if hit {
		return "hit"
	}
	return "miss"
}

// setWarmKeys records the count of distinct recently-warmed affinity keys routed to
// one replica of a model (M3 phase 1).
func (m *Metrics) setWarmKeys(model, worker string, count int) {
	if m != nil {
		m.warmKeys.WithLabelValues(model, worker).Set(float64(count))
	}
}

func (m *Metrics) incInFlight() {
	if m != nil {
		m.inFlight.Inc()
	}
}

func (m *Metrics) decInFlight() {
	if m != nil {
		m.inFlight.Dec()
	}
}

// MetricsSnapshot is the headline of the metrics registry for `atlas status`:
// totals computed by gathering the same series /metrics exposes, so the CLI and a
// scrape never disagree.
type MetricsSnapshot struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	InFlight     int64 `json:"in_flight"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	QueueDepth   int64 `json:"queue_depth"`
	Shed         int64 `json:"shed"`
	AffinityHits int64 `json:"affinity_hits"`
	AffinityMiss int64 `json:"affinity_miss"`
	WarmKeys     int64 `json:"warm_keys"`
}

// Snapshot summarizes the registry for the status endpoint. It gathers the live
// metric families and folds them into headline totals: requests (all statuses),
// errors (status >= 400), in-flight, and total input/output tokens. Reading
// through Gather is deliberate — it is exactly what a Prometheus scrape sees.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	families, err := m.reg.Gather()
	if err != nil {
		return MetricsSnapshot{}
	}
	var snap MetricsSnapshot
	for _, fam := range families {
		switch fam.GetName() {
		case metricRequests:
			for _, mc := range fam.GetMetric() {
				n := int64(mc.GetCounter().GetValue())
				snap.Requests += n
				if isErrorStatus(mc.GetLabel()) {
					snap.Errors += n
				}
			}
		case metricInFlight:
			for _, mc := range fam.GetMetric() {
				snap.InFlight += int64(mc.GetGauge().GetValue())
			}
		case metricInputTokens:
			snap.InputTokens += sumCounters(fam.GetMetric())
		case metricOutputTokens:
			snap.OutputTokens += sumCounters(fam.GetMetric())
		case metricQueueDepth:
			for _, mc := range fam.GetMetric() {
				snap.QueueDepth += int64(mc.GetGauge().GetValue())
			}
		case metricShed:
			snap.Shed += sumCounters(fam.GetMetric())
		case metricAffinity:
			for _, mc := range fam.GetMetric() {
				n := int64(mc.GetCounter().GetValue())
				if isAffinityHit(mc.GetLabel()) {
					snap.AffinityHits += n
				} else {
					snap.AffinityMiss += n
				}
			}
		case metricWarmKeys:
			for _, mc := range fam.GetMetric() {
				snap.WarmKeys += int64(mc.GetGauge().GetValue())
			}
		}
	}
	return snap
}

// isErrorStatus reports whether a requests_total sample's "status" label is an
// HTTP error (>= 400).
func isErrorStatus(labels []*dto.LabelPair) bool {
	for _, l := range labels {
		if l.GetName() == "status" {
			if code, err := strconv.Atoi(l.GetValue()); err == nil {
				return code >= 400
			}
		}
	}
	return false
}

// isAffinityHit reports whether an affinity_total sample's "result" label is a hit.
func isAffinityHit(labels []*dto.LabelPair) bool {
	for _, l := range labels {
		if l.GetName() == "result" {
			return l.GetValue() == "hit"
		}
	}
	return false
}

// sumCounters totals the counter values across a metric family's samples.
func sumCounters(metrics []*dto.Metric) int64 {
	var total int64
	for _, mc := range metrics {
		total += int64(mc.GetCounter().GetValue())
	}
	return total
}

// metricsPath collapses a request path to a bounded label set, so a path
// parameter (GET /v1/models/{id}) cannot blow up metric cardinality with one
// series per model id.
func metricsPath(p string) string {
	switch p {
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/models":
		return p
	}
	if strings.HasPrefix(p, "/v1/models/") {
		return "/v1/models/{id}"
	}
	return "other"
}
