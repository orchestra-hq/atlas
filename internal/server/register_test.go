package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// modelIDs fetches GET /v1/models and returns the listed ids.
func modelIDs(t *testing.T, srv *httptest.Server) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", testKey)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(body.Data))
	for i, m := range body.Data {
		ids[i] = m.ID
	}
	return ids
}

func readyStatus(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestRegisterUnregisterModel covers the gateway's dynamic model table: a model
// registered after construction (as the hub does when a worker joins) becomes
// servable, and unregistering it (worker drop) removes the route — the
// mechanism phase 2 relies on for routing to remote workers.
func TestRegisterUnregisterModel(t *testing.T) {
	g := NewGateway(testKey, nil, nil)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	// No models yet: not ready, nothing listed, a request 404s.
	if got := readyStatus(t, srv); got != http.StatusServiceUnavailable {
		t.Errorf("readyz with no models = %d, want 503", got)
	}
	if ids := modelIDs(t, srv); len(ids) != 0 {
		t.Errorf("models with none registered = %v, want empty", ids)
	}

	g.RegisterModel(Model{Name: "remote-a", Exec: &echoExecutor{reply: "hi"}, ContextWindow: 4096})

	if got := readyStatus(t, srv); got != http.StatusOK {
		t.Errorf("readyz after register = %d, want 200", got)
	}
	if ids := modelIDs(t, srv); len(ids) != 1 || ids[0] != "remote-a" {
		t.Errorf("models after register = %v, want [remote-a]", ids)
	}
	// The registered model is reachable end to end.
	resp, parsed := post(t, srv, testKey, `{"model":"remote-a","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages to registered model = %d (%v)", resp.StatusCode, parsed)
	}

	// Re-registering the same name updates in place rather than duplicating.
	g.RegisterModel(Model{Name: "remote-a", Exec: &echoExecutor{reply: "hi"}, ContextWindow: 8192})
	if ids := modelIDs(t, srv); len(ids) != 1 {
		t.Errorf("models after re-register = %v, want one entry", ids)
	}

	g.UnregisterModel("remote-a")
	g.UnregisterModel("never-registered") // no-op, must not panic

	if got := readyStatus(t, srv); got != http.StatusServiceUnavailable {
		t.Errorf("readyz after unregister = %d, want 503", got)
	}
	if ids := modelIDs(t, srv); len(ids) != 0 {
		t.Errorf("models after unregister = %v, want empty", ids)
	}
	resp, _ = post(t, srv, testKey, `{"model":"remote-a","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("messages to unregistered model = %d, want 404", resp.StatusCode)
	}
}

// TestAliasDispatchesUnderCanonicalName guards the alias-routing contract a
// remote worker depends on: a request addressed to an alias is dispatched to the
// executor under the canonical served name (the worker routes by req.Model),
// while the response still echoes the alias the client used.
func TestAliasDispatchesUnderCanonicalName(t *testing.T) {
	exec := &echoExecutor{reply: "ok"}
	g := NewGateway(testKey, []Model{{Name: "canon", Exec: exec, ContextWindow: 4096}}, map[string]string{"my-alias": "canon"})
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	resp, parsed := post(t, srv, testKey, `{"model":"my-alias","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%v)", resp.StatusCode, parsed)
	}
	if exec.gotReq.Model != "canon" {
		t.Errorf("executor received model %q, want canonical %q", exec.gotReq.Model, "canon")
	}
	if parsed["model"] != "my-alias" {
		t.Errorf("response model = %v, want the requested alias %q", parsed["model"], "my-alias")
	}
}
