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

// StreamExecutor is an Executor that can also stream a response incrementally.
// The in-process worker implements it; when an executor does not, the gateway
// falls back to buffering a non-streaming Execute and replaying it as a stream,
// so the SSE surface holds regardless of engine capability.
type StreamExecutor interface {
	Executor
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
}

// Gateway is the client-facing control plane: auth, model resolution, and
// dispatch to a worker. It serves POST /v1/messages both buffered and as an
// SSE stream (stream=true).
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

	if req.Stream {
		g.streamMessages(w, r, exec, coreReq, stops)
		return
	}

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
