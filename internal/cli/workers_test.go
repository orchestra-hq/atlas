package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkersListShowsStatus(t *testing.T) {
	const workers = `{"workers":[
		{"ID":"w_aaa","Name":"alpha","Hardware":{"platform":"cuda","ram_bytes":17179869184},"Version":"1.0","Draining":false},
		{"ID":"w_bbb","Name":"beta","Hardware":{"platform":"cpu","ram_bytes":8589934592},"Version":"1.0","Draining":true}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(workers))
	}))
	defer srv.Close()

	cmd := testCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runWorkersList(cmd, srv.URL); err != nil {
		t.Fatalf("runWorkersList: %v", err)
	}
	got := out.String()
	for _, want := range []string{"w_aaa", "alpha", "ready", "w_bbb", "beta", "draining", "STATUS"} {
		if !strings.Contains(got, want) {
			t.Errorf("workers list output missing %q:\n%s", want, got)
		}
	}
}

func TestWorkersRemoveSuccess(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	cmd := testCmd()
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runWorkersRemove(cmd, srv.URL, "w_xyz"); err != nil {
		t.Fatalf("runWorkersRemove: %v", err)
	}
	if gotPath != "POST /admin/workers/w_xyz/drain" {
		t.Errorf("request = %q, want POST /admin/workers/w_xyz/drain", gotPath)
	}
	if !strings.Contains(out.String(), "draining") {
		t.Errorf("expected draining confirmation:\n%s", out.String())
	}
}

func TestWorkersRemoveNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cmd := testCmd()
	cmd.SetContext(context.Background())
	err := runWorkersRemove(cmd, srv.URL, "w_missing")
	if err == nil {
		t.Fatal("expected error removing an unknown worker")
	}
	if !strings.Contains(err.Error(), "w_missing") {
		t.Errorf("error = %v, want it to name the missing worker", err)
	}
}
