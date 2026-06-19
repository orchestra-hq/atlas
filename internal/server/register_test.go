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

	g.RegisterInstance("w1", Model{Name: "remote-a", Exec: &echoExecutor{reply: "hi"}, ContextWindow: 4096})

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

	// A second instance of the same name adds a replica, not a second listing entry.
	g.RegisterInstance("w1", Model{Name: "remote-a", Exec: &echoExecutor{reply: "hi"}, ContextWindow: 4096})
	if ids := modelIDs(t, srv); len(ids) != 1 {
		t.Errorf("models after second instance = %v, want one entry", ids)
	}

	g.UnregisterWorker("w1")         // removes both of w1's instances
	g.UnregisterWorker("never-seen") // no-op, must not panic

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

// TestReconnectOverlapKeepsLiveRoute is the G11 reconnect case at the unit level:
// a worker serving a model reconnects (its new connection registers the model)
// before the old connection's teardown runs. Because routes carry worker
// identity, the old connection's UnregisterWorker removes only its own instance,
// so the model never transiently 404s — the bug a name-keyed registry had, where
// the stale teardown deleted the route the live connection had just installed.
func TestReconnectOverlapKeepsLiveRoute(t *testing.T) {
	g := NewGateway(testKey, nil, nil)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	// Old connection registers the model, then the reconnect's new connection
	// registers the same name under a fresh worker id (the overlap window).
	g.RegisterInstance("w_old", Model{Name: "m", Exec: &echoExecutor{reply: "old"}, ContextWindow: 4096})
	g.RegisterInstance("w_new", Model{Name: "m", Exec: &echoExecutor{reply: "new"}, ContextWindow: 4096})

	// The old connection's deferred teardown now fires.
	g.UnregisterWorker("w_old")

	// The model must still resolve and serve — the live (new) route survived.
	if got := readyStatus(t, srv); got != http.StatusOK {
		t.Errorf("readyz after stale teardown = %d, want 200 (live route must survive)", got)
	}
	resp, parsed := post(t, srv, testKey, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request after reconnect overlap = %d (%v), want 200", resp.StatusCode, parsed)
	}

	// Once the live connection drops too, the model is gone.
	g.UnregisterWorker("w_new")
	if got := readyStatus(t, srv); got != http.StatusServiceUnavailable {
		t.Errorf("readyz after last instance left = %d, want 503", got)
	}
}

// TestSameModelTwoWorkersKeepsRoute is the G11 same-model-overlap case at the
// unit level: two workers advertise the same model name. When one disconnects,
// requests for that model continue to be served by the other — neither worker's
// teardown drops the route the other owns.
func TestSameModelTwoWorkersKeepsRoute(t *testing.T) {
	g := NewGateway(testKey, nil, nil)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	g.RegisterInstance("w1", Model{Name: "shared", Exec: &echoExecutor{reply: "a"}, ContextWindow: 4096})
	g.RegisterInstance("w2", Model{Name: "shared", Exec: &echoExecutor{reply: "b"}, ContextWindow: 4096})

	// One worker drops; the model stays routable via the other.
	g.UnregisterWorker("w1")
	if ids := modelIDs(t, srv); len(ids) != 1 || ids[0] != "shared" {
		t.Errorf("models after one worker left = %v, want [shared]", ids)
	}
	resp, parsed := post(t, srv, testKey, `{"model":"shared","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request after one worker left = %d (%v), want 200", resp.StatusCode, parsed)
	}

	// The remaining worker drops; now the model is gone.
	g.UnregisterWorker("w2")
	resp, _ = post(t, srv, testKey, `{"model":"shared","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("request after both workers left = %d, want 404", resp.StatusCode)
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
