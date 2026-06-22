package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// AuditEvent is one control-plane mutation to record in the audit log (M3 phase 3).
// Actor is who performed it (an admin key id for an HTTP action); Action is a stable
// label; Target is the resource acted on; Result is "ok" or "error"; Detail holds
// optional context such as the HTTP status.
type AuditEvent struct {
	Actor  string
	Action string
	Target string
	Result string
	Detail string
}

// AuditRecorder persists audit events. The control-plane SQLite store implements it
// (bridged in the CLI), the same store the admin surface already uses — no new
// persistence dependency (build-plan decision 3). A nil recorder disables auditing,
// so the middleware is safe to wire unconditionally.
type AuditRecorder interface {
	RecordAudit(ctx context.Context, e AuditEvent) error
}

// auditBodyCap bounds how much of a mutation request body the audit middleware
// buffers to extract a target. Admin bodies are tiny (a deployment spec), so this is
// generous; a larger body is still served, only its target falls back to empty.
const auditBodyCap = 64 << 10 // 64 KiB

// adminIdentity authenticates the request as an admin-scoped key, writing the
// appropriate admin error and returning ok=false when it is not (missing/unknown/
// revoked → 401, valid but non-admin → 403, backend failure → 500). It is the shared
// gate for the admin surface, used by both RequireAdmin and RequireAdminAudited.
func adminIdentity(auth Authenticator, w http.ResponseWriter, r *http.Request) (Identity, bool) {
	secret := apiKeyFromRequest(r)
	if secret == "" {
		writeAdminError(w, http.StatusUnauthorized, "missing or invalid API key")
		return Identity{}, false
	}
	id, ok, err := auth.Authenticate(r.Context(), secret)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "authentication backend error")
		return Identity{}, false
	}
	if !ok {
		writeAdminError(w, http.StatusUnauthorized, "missing or invalid API key")
		return Identity{}, false
	}
	if !id.Admin {
		writeAdminError(w, http.StatusForbidden, "this API key is not permitted to use the admin surface")
		return Identity{}, false
	}
	return id, true
}

// RequireAdminAudited gates a mutating admin handler exactly as RequireAdmin does and
// then records the mutation in the audit log (M3 phase 3) — the single choke point
// for control-plane HTTP mutations. After the handler runs it writes an audit event
// with the acting admin key id as actor, the given action label, the affected target
// (a path id/model, or the "model" field of the request body), the result (ok/error
// by HTTP status), and the status in detail. Auditing failures are non-fatal: the
// mutation already happened and its response is already written, so a recorder error
// is swallowed (the recorder logs it). A nil recorder skips auditing.
func RequireAdminAudited(auth Authenticator, rec AuditRecorder, action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := adminIdentity(auth, w, r)
		if !ok {
			return
		}
		// Buffer the body so the target can be read from it and the handler can still
		// read it. Bodies are tiny; a read error leaves the handler with an empty body,
		// which it rejects on its own.
		body := bufferBody(r)

		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(sr, r)

		if rec == nil {
			return
		}
		result := "ok"
		if sr.status >= http.StatusBadRequest {
			result = "error"
		}
		_ = rec.RecordAudit(r.Context(), AuditEvent{
			Actor:  id.KeyID,
			Action: action,
			Target: auditTarget(r, body),
			Result: result,
			Detail: strconv.Itoa(sr.status),
		})
	}
}

// bufferBody reads up to auditBodyCap of the request body and restores r.Body so the
// wrapped handler reads the same bytes. Returns nil on a read failure or empty body.
func bufferBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, auditBodyCap))
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// auditTarget derives the resource a mutation acted on: a path-addressed id or model
// first ({id} for worker routes, {model} for deployment-stop), otherwise the "model"
// field of a JSON body (deployment-set, where the model is in the request body).
func auditTarget(r *http.Request, body []byte) string {
	if v := r.PathValue("id"); v != "" {
		return v
	}
	if v := r.PathValue("model"); v != "" {
		return v
	}
	if len(body) > 0 {
		var m struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &m) == nil && m.Model != "" {
			return m.Model
		}
	}
	return ""
}

// statusRecorder wraps a ResponseWriter to remember the status code written, so the
// audit middleware can record a mutation's result. A handler that never calls
// WriteHeader implies 200, the net/http default.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
