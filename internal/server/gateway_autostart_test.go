package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/orchestra-hq/atlas/catalog"
)

// fakeAutostarter stands in for the scheduler: on EnsureModel it registers a
// route (as the real hub does when a worker reports model_ready) so the
// gateway's retry resolves, and it records every EnsureModel/Touch call.
// classes is an optional map of model name → catalog class for ClassOf; an absent
// entry returns ok=false (unknown class, no pre-ensure rejection).
type fakeAutostarter struct {
	g       *Gateway
	exec    Executor
	succeed bool
	classes map[string]string // optional: catalog declared class per model

	mu      sync.Mutex
	ensured []string
	touched []string
}

func (f *fakeAutostarter) EnsureModel(_ context.Context, model string) bool {
	f.mu.Lock()
	f.ensured = append(f.ensured, model)
	ok := f.succeed
	f.mu.Unlock()
	if ok {
		cls := f.classes[model]
		f.g.RegisterInstance("auto", "auto", Model{Name: model, Exec: f.exec, Class: cls})
	}
	return ok
}

func (f *fakeAutostarter) Touch(model string) {
	f.mu.Lock()
	f.touched = append(f.touched, model)
	f.mu.Unlock()
}

func (f *fakeAutostarter) ClassOf(model string) (string, bool) {
	cls, ok := f.classes[model]
	return cls, ok
}

func (f *fakeAutostarter) ensuredModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ensured...)
}

func (f *fakeAutostarter) touchedModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.touched...)
}

func autostartServer(t *testing.T, aliases map[string]string, succeed bool) (*httptest.Server, *fakeAutostarter) {
	t.Helper()
	g := NewGateway(staticAuth(testKey), nil, aliases)
	fa := &fakeAutostarter{g: g, exec: &echoExecutor{reply: "hi"}, succeed: succeed}
	g.SetAutostarter(fa)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, fa
}

// TestAutostart_unroutedModelTriggersEnsure auto-starts a model with no live
// route: the request blocks on EnsureModel, which brings the route online, then
// completes against the new instance.
func TestAutostart_unroutedModelTriggersEnsure(t *testing.T) {
	srv, fa := autostartServer(t, nil, true)

	resp, body := post(t, srv, testKey, `{"model":"auto-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got := fa.ensuredModels(); len(got) != 1 || got[0] != "auto-model" {
		t.Fatalf("EnsureModel calls = %v, want [auto-model]", got)
	}
}

// TestAutostart_failureIsNotFound returns the normal model-not-found error when
// auto-start cannot bring the model online.
func TestAutostart_failureIsNotFound(t *testing.T) {
	srv, fa := autostartServer(t, nil, false)

	resp, body := post(t, srv, testKey, `{"model":"auto-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if got := fa.ensuredModels(); len(got) != 1 {
		t.Fatalf("EnsureModel calls = %v, want exactly one attempt", got)
	}
}

// TestAutostart_aliasEnsuresCanonical auto-starts the canonical target a client
// alias resolves to, not the alias name.
func TestAutostart_aliasEnsuresCanonical(t *testing.T) {
	srv, fa := autostartServer(t, map[string]string{"claude-sonnet-4-6": "served-model"}, true)

	resp, _ := post(t, srv, testKey, `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := fa.ensuredModels(); len(got) != 1 || got[0] != "served-model" {
		t.Fatalf("EnsureModel calls = %v, want [served-model] (the canonical target)", got)
	}
}

// TestAutostart_touchesAlreadyRoutedModel records activity (Touch) for a model
// that already has a route, so idle-stop sees it as in use, and does not
// re-trigger EnsureModel.
func TestAutostart_touchesAlreadyRoutedModel(t *testing.T) {
	srv, fa := autostartServer(t, nil, true)

	// First request auto-starts and registers the route.
	if resp, _ := post(t, srv, testKey, `{"model":"auto-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d", resp.StatusCode)
	}
	// Second request hits the live route: Touch, no second EnsureModel.
	if resp, _ := post(t, srv, testKey, `{"model":"auto-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d", resp.StatusCode)
	}
	if got := fa.ensuredModels(); len(got) != 1 {
		t.Fatalf("EnsureModel calls = %v, want only the first request to auto-start", got)
	}
	if got := fa.touchedModels(); len(got) != 1 || got[0] != "auto-model" {
		t.Fatalf("Touch calls = %v, want [auto-model] from the second (routed) request", got)
	}
}

// TestAutostart_coldWrongClassRejectedBeforeAutostart: a wrong-class request to a
// cold autostartable model must return a clean 400 without calling EnsureModel —
// no wasted autostart, no misleading 529.
func TestAutostart_coldWrongClassRejectedBeforeAutostart(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	fa := &fakeAutostarter{
		g:       g,
		exec:    &fakeEmbedder{vecs: [][]float32{{1}}},
		succeed: true,
		classes: map[string]string{"embed-model": catalog.ClassEmbedding},
	}
	g.SetAutostarter(fa)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	// Chat (POST /v1/messages) request against a cold embedding-class model.
	resp, body := post(t, srv, testKey, `{"model":"embed-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for cold wrong-class request (body %v)", resp.StatusCode, body)
	}
	if got := fa.ensuredModels(); len(got) != 0 {
		t.Fatalf("EnsureModel called %d times, want 0 (should reject before autostart)", len(got))
	}
}
