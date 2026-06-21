package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMetricsSnapshotReflectsRequests is the unit-level G15 case: requests and
// their token usage move the same counters /metrics exposes and the status
// snapshot summarizes, and an error status is counted as an error.
func TestMetricsSnapshotReflectsRequests(t *testing.T) {
	m := NewMetrics()
	g := NewGateway(staticAuth(testKey),
		[]Model{{Name: testModel, Exec: &echoExecutor{reply: "hi", outToken: 5}, ContextWindow: 4096}}, nil)
	g.SetMetrics(m)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	// One success and one not-found (an error status, no tokens).
	if resp, _ := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("success status = %d", resp.StatusCode)
	}
	if resp, _ := post(t, srv, testKey, `{"model":"missing","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("not-found status = %d", resp.StatusCode)
	}

	// observeRequest runs after the handler returns (the response may reach the
	// client first), so poll until both requests have landed.
	snap := waitForSnapshot(t, m, 2)
	if snap.Errors != 1 {
		t.Errorf("snapshot errors = %d, want 1 (the 404)", snap.Errors)
	}
	if snap.InputTokens <= 0 || snap.OutputTokens != 5 {
		t.Errorf("snapshot tokens = (%d,%d), want (>0, 5)", snap.InputTokens, snap.OutputTokens)
	}
	if snap.InFlight != 0 {
		t.Errorf("snapshot in-flight = %d after requests completed, want 0", snap.InFlight)
	}
}

// waitForSnapshot polls the metrics snapshot until at least wantRequests requests
// are recorded, returning it (the observe happens after the handler returns).
func waitForSnapshot(t *testing.T, m *Metrics, wantRequests int64) MetricsSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := m.Snapshot()
		if snap.Requests >= wantRequests {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot recorded %d requests, want >= %d", snap.Requests, wantRequests)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMetricsHandlerExposesSeries asserts the Prometheus exposition includes the
// Atlas series (and the connected-worker gauge reads its live source), and that
// the private registry carries no Go-runtime noise.
func TestMetricsHandlerExposesSeries(t *testing.T) {
	m := NewMetrics()
	m.SetWorkerCountSource(func() int { return 3 })
	m.observeRequest("/v1/messages", http.StatusOK, 12*time.Millisecond)
	m.addTokens(testModel, "gpu-box", 7, 5)

	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		metricRequests, metricRequestDuration, metricInputTokens, metricOutputTokens, metricInFlight,
		`atlas_connected_workers 3`,
		`atlas_input_tokens_total{model="test-model",worker="gpu-box"} 7`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "go_goroutines") {
		t.Error("/metrics leaked Go-runtime metrics; the private registry should carry only Atlas series")
	}
}

// TestMetricsPathBoundsCardinality guards the path-label normalization: a path
// parameter collapses to a single template label rather than one series per id.
func TestMetricsPathBoundsCardinality(t *testing.T) {
	cases := map[string]string{
		"/v1/messages":      "/v1/messages",
		"/v1/models":        "/v1/models",
		"/v1/models/abc":    "/v1/models/{id}",
		"/v1/models/qwen-7": "/v1/models/{id}",
		"/something/else":   "other",
	}
	for in, want := range cases {
		if got := metricsPath(in); got != want {
			t.Errorf("metricsPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNilMetricsSafe asserts every Metrics method is a no-op on nil, so a gateway
// with metering off (the default) can call them unconditionally.
func TestNilMetricsSafe(t *testing.T) {
	var m *Metrics
	m.observeRequest("/v1/messages", 200, time.Millisecond)
	m.addTokens("x", "y", 1, 1)
	m.incInFlight()
	m.decInFlight()
	m.SetWorkerCountSource(func() int { return 1 })
	if snap := m.Snapshot(); snap != (MetricsSnapshot{}) {
		t.Errorf("nil snapshot = %+v, want zero value", snap)
	}
}

// TestStatusHandler asserts the /admin/status snapshot combines workers,
// deployments, and the metrics headline into one JSON object.
func TestStatusHandler(t *testing.T) {
	m := NewMetrics()
	m.observeRequest("/v1/messages", http.StatusOK, time.Millisecond)

	workers := func() []WorkerInfo { return []WorkerInfo{{ID: "w_1", Name: "alpha"}} }
	deployments := func() []DeploymentInfo { return []DeploymentInfo{{Model: "m", Replicas: 2, Ready: 1, Pending: 1}} }

	srv := httptest.NewServer(StatusHandler(workers, deployments, m))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{`"Name":"alpha"`, `"model":"m"`, `"replicas":2`, `"requests":1`} {
		if !strings.Contains(text, want) {
			t.Errorf("status response missing %q:\n%s", want, text)
		}
	}
}
