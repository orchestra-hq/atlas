package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingAudit captures audit events for assertions.
type recordingAudit struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingAudit) RecordAudit(_ context.Context, e AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingAudit) last() (AuditEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return AuditEvent{}, false
	}
	return r.events[len(r.events)-1], true
}

// auditMux builds a mux with one audited route for the test, so path patterns
// ({id}/{model}) resolve exactly as in production.
func auditMux(rec AuditRecorder, pattern, action string, h http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, RequireAdminAudited(staticAuth(testKey), rec, action, h))
	return mux
}

func adminReq(t *testing.T, method, url, body string, withKey bool) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	if withKey {
		req.Header.Set("x-api-key", testKey)
	}
	return req
}

// TestAudited_recordsPathTarget: a worker-drain mutation records actor, action, the
// path-addressed target, and an ok result (G21).
func TestAudited_recordsPathTarget(t *testing.T) {
	rec := &recordingAudit{}
	mux := auditMux(rec, "POST /admin/workers/{id}/drain", "worker.drain",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminReq(t, "POST", "/admin/workers/w42/drain", "", true))

	ev, ok := rec.last()
	if !ok {
		t.Fatal("no audit event recorded")
	}
	if ev.Actor != "test" || ev.Action != "worker.drain" || ev.Target != "w42" || ev.Result != "ok" {
		t.Fatalf("event = %+v, want actor=test action=worker.drain target=w42 result=ok", ev)
	}
}

// TestAudited_recordsBodyTarget: a deployment-set mutation, whose model is in the
// request body, records that model as the target — and the handler still reads the
// same body (the middleware restores it).
func TestAudited_recordsBodyTarget(t *testing.T) {
	rec := &recordingAudit{}
	var handlerSawModel string
	mux := auditMux(rec, "POST /admin/deployments", "deployment.set",
		func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			handlerSawModel = body.Model
			w.WriteHeader(http.StatusOK)
		})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminReq(t, "POST", "/admin/deployments", `{"model":"qwen-7b","replicas":2}`, true))

	if handlerSawModel != "qwen-7b" {
		t.Fatalf("handler read model %q; the middleware did not restore the body", handlerSawModel)
	}
	ev, _ := rec.last()
	if ev.Action != "deployment.set" || ev.Target != "qwen-7b" {
		t.Fatalf("event = %+v, want deployment.set on qwen-7b", ev)
	}
}

// TestAudited_recordsErrorResult: a handler that fails records result=error with the
// status in detail.
func TestAudited_recordsErrorResult(t *testing.T) {
	rec := &recordingAudit{}
	mux := auditMux(rec, "DELETE /admin/deployments/{model}", "deployment.stop",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminReq(t, "DELETE", "/admin/deployments/m1", "", true))

	ev, _ := rec.last()
	if ev.Result != "error" || ev.Detail != "409" {
		t.Fatalf("event = %+v, want result=error detail=409", ev)
	}
}

// TestAudited_noRecordWhenUnauthorized: a request rejected by the admin gate never
// reaches the handler and records nothing (the mutation did not happen).
func TestAudited_noRecordWhenUnauthorized(t *testing.T) {
	rec := &recordingAudit{}
	called := false
	mux := auditMux(rec, "POST /admin/workers/{id}/drain", "worker.drain",
		func(http.ResponseWriter, *http.Request) { called = true })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, adminReq(t, "POST", "/admin/workers/w1/drain", "", false)) // no key

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if called {
		t.Fatal("handler ran despite failed auth")
	}
	if _, ok := rec.last(); ok {
		t.Fatal("an unauthorized request was audited; only real mutations should be")
	}
}
