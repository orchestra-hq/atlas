package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/server"
)

// G12 (auth, M1 phase 5) at the integration level: the real SQLite key store,
// the production keyAuth adapter, and a real gateway, exercising the full client
// auth contract from docs/conformance-suite.md G12. The unit-level pieces live
// in internal/db (store) and internal/server (gateway 401/403/500); this test
// proves they compose — including that a revoke through the store is visible to a
// live gateway on the very next request, with no restart or cache window.

// fixedExecutor is a trivial server.Executor: it returns a canned reply so an
// authenticated request reaches a 200 without a real engine.
type fixedExecutor struct{}

func (fixedExecutor) Execute(_ context.Context, _ core.Request) (core.Response, error) {
	return core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock("ok")},
		StopReason: core.StopEndTurn,
		Usage:      core.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

// authPost sends a /v1/messages request for model with the given key (omitted
// when empty) and returns the status code.
func authPost(t *testing.T, srv *httptest.Server, key, model string) int {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestG12_ClientAuthContract(t *testing.T) {
	ctx := context.Background()
	store, err := openStateDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	// A full-access key and a key restricted to model-a.
	fullKey, _, err := store.CreateKey(ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	limitedKey, limited, err := store.CreateKey(ctx, []string{"model-a"}, false)
	if err != nil {
		t.Fatal(err)
	}

	gw := server.NewGateway(keyAuth{db: store}, []server.Model{
		{Name: "model-a", Exec: fixedExecutor{}},
		{Name: "model-b", Exec: fixedExecutor{}},
	}, nil)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	cases := []struct {
		name  string
		key   string
		model string
		want  int
	}{
		{"missing key → 401", "", "model-a", http.StatusUnauthorized},
		{"invalid key → 401", "atlas-bogus", "model-a", http.StatusUnauthorized},
		{"valid key → 200", fullKey, "model-a", http.StatusOK},
		{"allowlisted model → 200", limitedKey, "model-a", http.StatusOK},
		{"disallowed model → 403", limitedKey, "model-b", http.StatusForbidden},
	}
	for _, tc := range cases {
		if got := authPost(t, srv, tc.key, tc.model); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}

	// Revocation takes effect immediately: the same key that just succeeded is
	// rejected on the next request, no restart, no cache window.
	if got := authPost(t, srv, limitedKey, "model-a"); got != http.StatusOK {
		t.Fatalf("pre-revoke: status = %d, want 200", got)
	}
	if err := store.RevokeKey(ctx, limited.ID); err != nil {
		t.Fatal(err)
	}
	if got := authPost(t, srv, limitedKey, "model-a"); got != http.StatusUnauthorized {
		t.Errorf("post-revoke: status = %d, want 401 (immediate revocation)", got)
	}
}
