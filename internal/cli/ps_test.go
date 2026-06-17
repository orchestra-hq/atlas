package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGateway stands up just enough of the Atlas HTTP surface for ps to probe:
// /healthz, /readyz, and an authed /v1/models.
func fakeGateway(t *testing.T, ready bool, apiKey, modelsJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(modelsJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func runPsCapture(t *testing.T, opts *psOptions) (string, error) {
	t.Helper()
	cmd := testCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runPs(context.Background(), cmd, opts)
	return out.String(), err
}

func TestPsListsModelsAndAliases(t *testing.T) {
	const models = `{"data":[
		{"type":"model","id":"qwen3-0.6b","display_name":"qwen3-0.6b","context_window":40960},
		{"type":"model","id":"claude-sonnet-4-6","display_name":"qwen3-0.6b","context_window":40960}
	],"has_more":false}`
	srv := fakeGateway(t, true, "k", models)

	out, err := runPsCapture(t, &psOptions{addr: strings.TrimPrefix(srv.URL, "http://"), apiKey: "k"})
	if err != nil {
		t.Fatalf("runPs: %v", err)
	}
	for _, want := range []string{"ready", "qwen3-0.6b", "40960", "claude-sonnet-4-6"} {
		if !strings.Contains(out, want) {
			t.Errorf("ps output missing %q:\n%s", want, out)
		}
	}
	// The alias row shows what it resolves to; the canonical row does not.
	if strings.Count(out, "qwen3-0.6b") < 2 {
		t.Errorf("expected alias to show its resolved model:\n%s", out)
	}
}

func TestPsReportsNotReady(t *testing.T) {
	srv := fakeGateway(t, false, "k", `{"data":[],"has_more":false}`)
	out, err := runPsCapture(t, &psOptions{addr: strings.TrimPrefix(srv.URL, "http://"), apiKey: "k"})
	if err != nil {
		t.Fatalf("runPs: %v", err)
	}
	if !strings.Contains(out, "not ready") {
		t.Errorf("expected not-ready status:\n%s", out)
	}
}

func TestPsNoInstance(t *testing.T) {
	// An address with nothing listening should fail fast with a clear message.
	if _, err := runPsCapture(t, &psOptions{addr: "127.0.0.1:0", apiKey: "k"}); err == nil {
		t.Fatal("expected error when no instance is reachable")
	}
}

func TestPsRequiresKeyForModels(t *testing.T) {
	srv := fakeGateway(t, true, "k", `{"data":[],"has_more":false}`)
	_, err := runPsCapture(t, &psOptions{addr: strings.TrimPrefix(srv.URL, "http://"), apiKey: ""})
	if err == nil {
		t.Fatal("expected error listing models without a key")
	}
}
