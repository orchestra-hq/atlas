// Package server is the control plane: the client-facing gateway (auth,
// routing, and — from later phases — SSE), plus the worker registry and
// scheduler that stay trivial in M0 single-node mode (ADR-0003).
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
	"github.com/orchestra-hq/atlas/internal/core"
)

// Executor runs one inference request to completion. It is the gateway's view
// of a worker: in M0 the single in-process worker implements it directly (the
// wire protocol for remote workers is an M1 decision — ADR build-note 4).
type Executor interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
}

// Gateway is the client-facing control plane: auth, model resolution, and
// dispatch to a worker. Phase 2 serves non-streaming POST /v1/messages only.
type Gateway struct {
	apiKey string
	models map[string]Executor
}

// NewGateway builds a gateway that accepts apiKey and resolves each model name
// in models to the executor that serves it. In M0 single-node mode every name
// maps to the one in-process worker.
func NewGateway(apiKey string, models map[string]Executor) *Gateway {
	return &Gateway{apiKey: apiKey, models: models}
}

// Handler returns the gateway's HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", g.handleMessages)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		anthropic.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// maxRequestBytes caps a request body. Generous for chat; a hard ceiling so a
// malformed length header can't exhaust memory.
const maxRequestBytes = 32 << 20 // 32 MiB

func (g *Gateway) handleMessages(w http.ResponseWriter, r *http.Request) {
	if !g.authenticated(r) {
		anthropic.WriteError(w, &anthropic.Error{
			Status: http.StatusUnauthorized,
			Type:   anthropic.ErrAuthentication,
			Msg:    "missing or invalid API key",
		})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("could not read request body"))
		return
	}

	var req anthropic.MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("request body is not valid JSON"))
		return
	}

	if req.Stream {
		anthropic.WriteError(w, anthropic.ErrInvalid("streaming is not implemented yet (lands in m0-build-plan phase 3)"))
		return
	}

	coreReq, err := req.ToCore()
	if err != nil {
		writeErr(w, err)
		return
	}

	exec, ok := g.models[coreReq.Model]
	if !ok {
		anthropic.WriteError(w, &anthropic.Error{
			Status: http.StatusNotFound,
			Type:   anthropic.ErrNotFound,
			Msg:    "model not found: " + coreReq.Model,
		})
		return
	}

	// The gateway owns stop-sequence semantics, so the engine never sees them
	// and behavior is identical across engines.
	stops := coreReq.StopSequences
	coreReq.StopSequences = nil

	resp, err := exec.Execute(r.Context(), coreReq)
	if err != nil {
		writeErr(w, err)
		return
	}

	resp, seq := core.ApplyStopSequences(resp, stops)
	var stopSeq *string
	if seq != "" {
		stopSeq = &seq
	}

	anthropic.WriteJSON(w, http.StatusOK, anthropic.FromCore(newMessageID(), coreReq.Model, resp, stopSeq))
}

// authenticated checks the key from x-api-key or Authorization: Bearer
// (clients vary — docs/api-surface.md), constant-time.
func (g *Gateway) authenticated(r *http.Request) bool {
	key := r.Header.Get("x-api-key")
	if key == "" {
		if after, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
			key = after
		}
	}
	if key == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(g.apiKey)) == 1
}

// writeErr renders an *anthropic.Error as its envelope; anything else becomes
// a 500 api_error (engine/internal failures the client shouldn't see details of).
func writeErr(w http.ResponseWriter, err error) {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		anthropic.WriteError(w, apiErr)
		return
	}
	anthropic.WriteError(w, &anthropic.Error{
		Status: http.StatusInternalServerError,
		Type:   anthropic.ErrAPI,
		Msg:    "internal error",
	})
}

func newMessageID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}
