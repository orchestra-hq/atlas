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
	"sort"
	"strings"
	"time"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
	"github.com/orchestra-hq/atlas/internal/core"
)

// statusOverloaded is the non-standard 529 status Anthropic uses for transient
// overload; net/http has no constant for it. SDK retry logic keys on it.
const statusOverloaded = 529

// Executor runs one inference request to completion. It is the gateway's view
// of a worker: in M0 the single in-process worker implements it directly (the
// wire protocol for remote workers is an M1 decision — ADR build-note 4).
type Executor interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
}

// StreamExecutor is an Executor that can also stream a response incrementally.
// The in-process worker implements it; when an executor does not, the gateway
// falls back to buffering a non-streaming Execute and replaying it as a stream,
// so the SSE surface holds regardless of engine capability.
type StreamExecutor interface {
	Executor
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
}

// TokenCounter is an Executor that can count a request's prompt tokens using
// the engine's real tokenizer. The gateway uses it for POST
// /v1/messages/count_tokens and to assert context-window fit before dispatch
// (docs/m0-acceptance.md). The in-process worker implements it; an executor
// that does not simply skips the pre-dispatch assertion.
type TokenCounter interface {
	CountTokens(ctx context.Context, req core.Request) (int, error)
}

// Model is one model the gateway serves: a canonical Name, the Executor that
// runs it, and its ContextWindow in tokens (0 = unknown, assertion skipped).
type Model struct {
	Name          string
	Exec          Executor
	ContextWindow int
}

// Gateway is the client-facing control plane: auth, model resolution, and
// dispatch to a worker. It serves the Anthropic surface: POST /v1/messages
// (buffered and SSE), POST /v1/messages/count_tokens, and GET /v1/models[/{id}].
type Gateway struct {
	apiKey    string
	models    map[string]Model  // canonical name -> model
	aliases   map[string]string // alias -> canonical name
	order     []string          // canonical names, registration order (listing)
	createdAt string            // wire created_at stamped on model objects
}

// NewGateway builds a gateway that accepts apiKey, serves each model in models
// by its Name, and resolves each alias to a canonical model name. In M0
// single-node mode every model maps to one in-process worker; operator-defined
// aliases (e.g. claude-sonnet-4-6 -> a local model) let SDK/tool defaults
// resolve (docs/api-surface.md).
func NewGateway(apiKey string, models []Model, aliases map[string]string) *Gateway {
	byName := make(map[string]Model, len(models))
	order := make([]string, 0, len(models))
	for _, m := range models {
		byName[m.Name] = m
		order = append(order, m.Name)
	}
	if aliases == nil {
		aliases = map[string]string{}
	}
	return &Gateway{
		apiKey:    apiKey,
		models:    byName,
		aliases:   aliases,
		order:     order,
		createdAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// resolve maps a requested model name (alias or canonical) to its Model.
func (g *Gateway) resolve(name string) (Model, bool) {
	if canon, ok := g.aliases[name]; ok {
		name = canon
	}
	m, ok := g.models[name]
	return m, ok
}

// Handler returns the gateway's HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", g.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", g.handleCountTokens)
	mux.HandleFunc("GET /v1/models", g.handleListModels)
	mux.HandleFunc("GET /v1/models/{id}", g.handleGetModel)
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
		writeUnauthorized(w)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("could not read request body"))
		return
	}

	var req anthropic.MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("request body is not valid JSON"))
		return
	}

	coreReq, err := req.ToCore()
	if err != nil {
		writeErr(w, err)
		return
	}

	model, ok := g.resolve(coreReq.Model)
	if !ok {
		writeModelNotFound(w, coreReq.Model)
		return
	}

	// Assert the prompt fits the model's window before dispatch, so an oversized
	// request fails with a clean 400 rather than a garbled engine overflow
	// (docs/m0-acceptance.md context-window handling).
	if err := g.assertContextFits(r.Context(), model, coreReq); err != nil {
		writeErr(w, err)
		return
	}

	// The gateway owns stop-sequence semantics, so the engine never sees them
	// and behavior is identical across engines.
	stops := coreReq.StopSequences
	coreReq.StopSequences = nil

	if req.Stream {
		g.streamMessages(w, r, model.Exec, coreReq, stops)
		return
	}

	resp, err := model.Exec.Execute(r.Context(), coreReq)
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

// assertContextFits rejects a request whose prompt plus max_tokens cannot fit
// the model's context window. The window is the engine's real n_ctx; the count
// is the engine's real tokenizer. A max_tokens that alone meets the window is
// always too big and is caught without an engine round-trip. Counting is
// best-effort: if the tokenizer call fails (and the engine is truly down,
// dispatch surfaces that as a 529), the request proceeds rather than being
// blocked on a transient hiccup.
func (g *Gateway) assertContextFits(ctx context.Context, model Model, req core.Request) error {
	if model.ContextWindow <= 0 {
		return nil // unknown window: nothing to assert against
	}
	if req.MaxTokens >= model.ContextWindow {
		return anthropic.ErrInvalid("max_tokens (%d) exceeds the model's %d-token context window", req.MaxTokens, model.ContextWindow)
	}
	tc, ok := model.Exec.(TokenCounter)
	if !ok {
		return nil
	}
	n, err := tc.CountTokens(ctx, req)
	if err != nil {
		return nil // best-effort; see doc comment
	}
	if n+req.MaxTokens > model.ContextWindow {
		return anthropic.ErrInvalid(
			"prompt is too long: %d input tokens + %d max_tokens exceeds the model's %d-token context window",
			n, req.MaxTokens, model.ContextWindow)
	}
	return nil
}

// handleCountTokens serves POST /v1/messages/count_tokens: the prompt's token
// count from the target model's real tokenizer (criterion 5).
func (g *Gateway) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !g.authenticated(r) {
		writeUnauthorized(w)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("could not read request body"))
		return
	}

	var req anthropic.CountTokensRequest
	if err := json.Unmarshal(body, &req); err != nil {
		anthropic.WriteError(w, anthropic.ErrInvalid("request body is not valid JSON"))
		return
	}

	coreReq, err := req.ToCore()
	if err != nil {
		writeErr(w, err)
		return
	}

	model, ok := g.resolve(coreReq.Model)
	if !ok {
		writeModelNotFound(w, coreReq.Model)
		return
	}

	tc, ok := model.Exec.(TokenCounter)
	if !ok {
		anthropic.WriteError(w, &anthropic.Error{Status: http.StatusInternalServerError, Type: anthropic.ErrAPI, Msg: "count_tokens unsupported for this model"})
		return
	}
	n, err := tc.CountTokens(r.Context(), coreReq)
	if err != nil {
		writeErr(w, err)
		return
	}

	anthropic.WriteJSON(w, http.StatusOK, anthropic.CountTokensResponse{InputTokens: n})
}

// handleListModels serves GET /v1/models: every deployed model followed by
// every alias, each with context-window metadata (criterion 4).
func (g *Gateway) handleListModels(w http.ResponseWriter, r *http.Request) {
	if !g.authenticated(r) {
		writeUnauthorized(w)
		return
	}

	infos := make([]anthropic.ModelInfo, 0, len(g.order)+len(g.aliases))
	for _, name := range g.order {
		infos = append(infos, g.modelInfo(name))
	}
	aliasNames := make([]string, 0, len(g.aliases))
	for a := range g.aliases {
		aliasNames = append(aliasNames, a)
	}
	sort.Strings(aliasNames)
	for _, a := range aliasNames {
		infos = append(infos, g.modelInfo(a))
	}

	anthropic.WriteJSON(w, http.StatusOK, anthropic.NewModelList(infos))
}

// handleGetModel serves GET /v1/models/{id} for an alias or a canonical name.
func (g *Gateway) handleGetModel(w http.ResponseWriter, r *http.Request) {
	if !g.authenticated(r) {
		writeUnauthorized(w)
		return
	}

	id := r.PathValue("id")
	if _, ok := g.resolve(id); !ok {
		writeModelNotFound(w, id)
		return
	}
	anthropic.WriteJSON(w, http.StatusOK, g.modelInfo(id))
}

// modelInfo builds the wire model object for an alias or canonical id. The id
// echoes what the client addressed; display_name is the canonical model it
// resolves to, and context_window is that model's window. The caller must have
// confirmed id resolves.
func (g *Gateway) modelInfo(id string) anthropic.ModelInfo {
	model, _ := g.resolve(id)
	return anthropic.NewModelInfo(id, model.Name, g.createdAt, model.ContextWindow)
}

// streamMessages serves a streaming POST /v1/messages. It opens the SSE
// response, drives the executor (native streaming if supported, else a
// buffered Execute replayed as one stream), and applies stop sequences as text
// arrives so behavior matches the non-streaming path.
//
// Once the SSE headers are written the status is committed: a mid-stream engine
// failure becomes an error event, not an HTTP error.
func (g *Gateway) streamMessages(w http.ResponseWriter, r *http.Request, exec Executor, req core.Request, stops []string) {
	sw, err := anthropic.NewStreamWriter(w, newMessageID(), req.Model)
	if err != nil {
		anthropic.WriteError(w, &anthropic.Error{Status: http.StatusInternalServerError, Type: anthropic.ErrAPI, Msg: "streaming unsupported"})
		return
	}
	if err := sw.Start(0); err != nil {
		return // client went away; nothing more we can do
	}

	sink := &streamSink{sw: sw, scanner: core.NewStopSequenceScanner(stops), reason: core.StopEndTurn}

	if streamer, ok := exec.(StreamExecutor); ok {
		err = streamer.ExecuteStream(r.Context(), req, sink)
	} else {
		err = bufferedStream(r.Context(), exec, req, sink)
	}
	if err != nil {
		_ = sw.Error(anthropic.ErrAPI, "engine error during generation")
		return
	}

	_ = sw.Finish(sink.reason, sink.stopSeq, sink.usage)
}

// bufferedStream adapts a non-streaming Executor to the streaming sink by
// running Execute and replaying each content block as the matching sink events,
// then Done. It preserves block order so a text-then-tool_use response streams
// the same shape a native streamer would produce.
func bufferedStream(ctx context.Context, exec Executor, req core.Request, sink core.StreamSink) error {
	resp, err := exec.Execute(ctx, req)
	if err != nil {
		return err
	}
	for i, b := range resp.Blocks {
		switch b.Type {
		case core.BlockThinking:
			if b.Thinking == "" {
				continue
			}
			if err := sink.Thinking(b.Thinking); err != nil {
				return err
			}
		case core.BlockText:
			if b.Text == "" {
				continue
			}
			if err := sink.Text(b.Text); err != nil {
				if errors.Is(err, core.ErrStopStreaming) {
					return nil
				}
				return err
			}
		case core.BlockToolUse:
			if err := sink.ToolCallStart(i, b.ID, b.Name); err != nil {
				return err
			}
			if err := sink.ToolCallDelta(i, string(b.Input)); err != nil {
				return err
			}
		}
	}
	return sink.Done(resp.StopReason, resp.Usage)
}

// streamSink interposes between an engine's deltas and the SSE writer: it runs
// text through the stop-sequence scanner (truncating and ending the stream when
// one matches) and records the final stop reason, sequence, and usage for the
// closing message_delta.
type streamSink struct {
	sw      *anthropic.StreamWriter
	scanner *core.StopSequenceScanner
	reason  core.StopReason
	stopSeq *string
	usage   core.Usage
}

// Thinking forwards a reasoning delta straight to the writer. Stop sequences
// match the model's visible answer, not its reasoning, so thinking text bypasses
// the scanner (and reasoning always precedes text, so the scanner is still empty
// here anyway).
func (s *streamSink) Thinking(delta string) error {
	return s.sw.ThinkingDelta(delta)
}

func (s *streamSink) Text(delta string) error {
	emit, matched := s.scanner.Push(delta)
	if emit != "" {
		if err := s.sw.TextDelta(emit); err != nil {
			return err
		}
	}
	if matched {
		s.reason = core.StopStopSequence
		seq := s.scanner.Matched()
		s.stopSeq = &seq
		return core.ErrStopStreaming
	}
	return nil
}

// ToolCallStart opens a tool_use block. Stop sequences apply to model text, not
// tool arguments, so any text held back by the scanner is flushed first to keep
// it ahead of the tool block on the wire.
func (s *streamSink) ToolCallStart(_ int, id, name string) error {
	if tail := s.scanner.Flush(); tail != "" {
		if err := s.sw.TextDelta(tail); err != nil {
			return err
		}
	}
	s.reason = core.StopToolUse
	return s.sw.ToolUseStart(id, name)
}

func (s *streamSink) ToolCallDelta(_ int, argsFragment string) error {
	return s.sw.ToolUseDelta(argsFragment)
}

func (s *streamSink) Done(reason core.StopReason, usage core.Usage) error {
	if tail := s.scanner.Flush(); tail != "" {
		if err := s.sw.TextDelta(tail); err != nil {
			return err
		}
	}
	s.reason = reason
	s.usage = usage
	return nil
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

// readBody reads a request body under the size cap.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
}

func writeUnauthorized(w http.ResponseWriter) {
	anthropic.WriteError(w, &anthropic.Error{
		Status: http.StatusUnauthorized,
		Type:   anthropic.ErrAuthentication,
		Msg:    "missing or invalid API key",
	})
}

func writeModelNotFound(w http.ResponseWriter, model string) {
	anthropic.WriteError(w, &anthropic.Error{
		Status: http.StatusNotFound,
		Type:   anthropic.ErrNotFound,
		Msg:    "model not found: " + model,
	})
}

// writeErr renders an error as its Anthropic envelope. An *anthropic.Error
// carries its own status; an engine-unavailable failure (core.ErrEngineUnavailable)
// becomes a retryable 529 overloaded_error; anything else is a 500 api_error
// (internal failures the client shouldn't see details of).
func writeErr(w http.ResponseWriter, err error) {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		anthropic.WriteError(w, apiErr)
		return
	}
	if errors.Is(err, core.ErrEngineUnavailable) {
		anthropic.WriteError(w, &anthropic.Error{
			Status: statusOverloaded,
			Type:   anthropic.ErrOverloaded,
			Msg:    "the inference engine is unavailable; retry shortly",
		})
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
