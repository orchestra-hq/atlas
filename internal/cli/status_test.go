package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/server"
)

// TestStatusRenders asserts `atlas status` decodes the /admin/status snapshot and
// renders the worker, deployment, and metrics sections (M2 phase 1, G15).
func TestStatusRenders(t *testing.T) {
	status := server.FleetStatus{
		Workers:     []server.WorkerInfo{{ID: "w_aaa", Name: "alpha", Models: []string{"qwen"}}},
		Deployments: []server.DeploymentInfo{{Model: "qwen", Replicas: 2, Ready: 1, Pending: 1}},
		Metrics:     server.MetricsSnapshot{Requests: 10, Errors: 2, InFlight: 1, InputTokens: 100, OutputTokens: 50, QueueDepth: 4, Shed: 9},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer srv.Close()

	client, err := newAdminClient(srv.URL, "", "")
	if err != nil {
		t.Fatalf("newAdminClient: %v", err)
	}
	cmd := testCmd()
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runStatus(cmd, client, false); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"10 total", "2 errors", "1 in flight", "100 input", "4 queued", "9 shed", "alpha", "qwen", "DEPLOYMENTS"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
}

// TestStatusJSON asserts --json passes the snapshot through verbatim.
func TestStatusJSON(t *testing.T) {
	status := server.FleetStatus{Metrics: server.MetricsSnapshot{Requests: 7}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer srv.Close()

	client, err := newAdminClient(srv.URL, "", "")
	if err != nil {
		t.Fatalf("newAdminClient: %v", err)
	}
	cmd := testCmd()
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runStatus(cmd, client, true); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !strings.Contains(out.String(), `"requests": 7`) {
		t.Errorf("--json output missing requests field:\n%s", out.String())
	}
}
