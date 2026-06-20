package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
	"github.com/orchestra-hq/atlas/internal/api/openai"
	"github.com/orchestra-hq/atlas/internal/core"
)

// handleChatCompletions serves POST /v1/chat/completions, Atlas's
// OpenAI-compatible surface. It reuses the same auth, model resolution,
// context-window assertion, and gateway-owned stop-sequence semantics as the
// Anthropic path — only the wire shapes differ (build-time decision 1).
func (g *Gateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	id, authErr := g.authenticate(r)
	if authErr != nil {
		writeOpenAIErr(w, authErr)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		openai.WriteError(w, openai.ErrInvalid("could not read request body"))
		return
	}

	var req openai.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		openai.WriteError(w, openai.ErrInvalid("request body is not valid JSON"))
		return
	}

	coreReq, err := req.ToCore()
	if err != nil {
		writeOpenAIErr(w, err)
		return
	}

	if !g.modelPermitted(id, coreReq.Model) {
		writeOpenAIErr(w, forbiddenModelErr(coreReq.Model))
		return
	}

	model, worker, ok := g.resolveOrStart(r.Context(), coreReq.Model)
	if !ok {
		writeOpenAIModelNotFound(w, coreReq.Model)
		return
	}

	if err := g.assertContextFits(r.Context(), model, coreReq); err != nil {
		writeOpenAIErr(w, err)
		return
	}

	// The gateway owns stop-sequence semantics; the engine never sees them.
	stops := coreReq.StopSequences
	coreReq.StopSequences = nil

	// The client may have addressed an alias; dispatch under the canonical served
	// name (a remote worker routes by req.Model) but echo the requested name back.
	requested := coreReq.Model
	coreReq.Model = model.Name

	tags := usageTags{keyID: id.KeyID, workerID: worker}

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		g.streamChatCompletion(w, r, model.Exec, coreReq, requested, stops, includeUsage, tags)
		return
	}

	resp, err := model.Exec.Execute(r.Context(), coreReq)
	if err != nil {
		writeOpenAIErr(w, err)
		return
	}

	resp, _ = core.ApplyStopSequences(resp, stops)
	recordBillableUsage(r.Context(), tags, requested, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	openai.WriteJSON(w, http.StatusOK, openai.FromCore(newCompletionID(), time.Now().Unix(), requested, resp))
}

// streamChatCompletion serves a streaming POST /v1/chat/completions. It opens
// the SSE response and drives the executor (native streaming if supported, else
// a buffered Execute replayed as one stream), applying stop sequences as text
// arrives so behavior matches the non-streaming path. Once the headers are
// written the status is committed: a mid-stream engine failure becomes an error
// event, not an HTTP error.
// req is the request to dispatch (its Model is the canonical served name);
// echoModel is the name the client addressed (possibly an alias), echoed back on
// the stream and in usage.
func (g *Gateway) streamChatCompletion(w http.ResponseWriter, r *http.Request, exec Executor, req core.Request, echoModel string, stops []string, includeUsage bool, tags usageTags) {
	sw, err := openai.NewStreamWriter(w, newCompletionID(), time.Now().Unix(), echoModel)
	if err != nil {
		openai.WriteError(w, &openai.Error{Status: http.StatusInternalServerError, Type: openai.ErrAPI, Msg: "streaming unsupported"})
		return
	}
	if err := sw.Role(); err != nil {
		return // client went away
	}

	sink := &openaiStreamSink{sw: sw, scanner: core.NewStopSequenceScanner(stops), reason: core.StopEndTurn}

	if streamer, ok := exec.(StreamExecutor); ok {
		err = streamer.ExecuteStream(r.Context(), req, sink)
	} else {
		err = bufferedStream(r.Context(), exec, req, sink)
	}
	if err != nil {
		// Interrupted mid-stream: record the usage emitted up to the cut rather
		// than zero, matching the Anthropic path (G13 interrupted case). A stream
		// that produced nothing records nothing.
		if out := sink.partialOutputTokens(); out > 0 {
			recordBillableUsage(r.Context(), tags, echoModel, sink.usage.InputTokens, out)
		}
		_ = sw.Error(openai.ErrAPI, "engine error during generation")
		return
	}

	recordBillableUsage(r.Context(), tags, echoModel, sink.usage.InputTokens, sink.usage.OutputTokens)
	_ = sw.Finish(sink.reason, sink.usage, includeUsage)
}

// openaiStreamSink translates core stream events into OpenAI chunk events,
// running text through the stop-sequence scanner so a matched sequence
// truncates output and ends the stream the same way the Anthropic path does.
type openaiStreamSink struct {
	sw       *openai.StreamWriter
	scanner  *core.StopSequenceScanner
	reason   core.StopReason
	usage    core.Usage
	outBytes int // emitted output text bytes, for the interrupted-stream estimate
}

// partialOutputTokens mirrors streamSink.partialOutputTokens for the OpenAI
// path: the engine's exact count if known, else an estimate from emitted bytes,
// so an interrupted stream records what it produced rather than zero (G13).
func (s *openaiStreamSink) partialOutputTokens() int {
	if s.usage.OutputTokens > 0 {
		return s.usage.OutputTokens
	}
	return estimateTokens(s.outBytes)
}

// Thinking is dropped: the OpenAI chat surface does not carry reasoning. (The
// adapter already suppresses reasoning when the request has no thinking config,
// which an OpenAI request never sets, so this is a defensive no-op.)
func (s *openaiStreamSink) Thinking(string) error { return nil }

func (s *openaiStreamSink) Text(delta string) error {
	emit, matched := s.scanner.Push(delta)
	if emit != "" {
		if err := s.sw.TextDelta(emit); err != nil {
			return err
		}
		s.outBytes += len(emit)
	}
	if matched {
		s.reason = core.StopStopSequence
		return core.ErrStopStreaming
	}
	return nil
}

func (s *openaiStreamSink) ToolCallStart(index int, id, name string) error {
	if tail := s.scanner.Flush(); tail != "" {
		if err := s.sw.TextDelta(tail); err != nil {
			return err
		}
	}
	s.reason = core.StopToolUse
	return s.sw.ToolCallStart(index, id, name)
}

func (s *openaiStreamSink) ToolCallDelta(index int, argsFragment string) error {
	return s.sw.ToolCallDelta(index, argsFragment)
}

func (s *openaiStreamSink) Done(reason core.StopReason, usage core.Usage) error {
	if tail := s.scanner.Flush(); tail != "" {
		if err := s.sw.TextDelta(tail); err != nil {
			return err
		}
	}
	s.reason = reason
	s.usage = usage
	return nil
}

func writeOpenAIModelNotFound(w http.ResponseWriter, model string) {
	openai.WriteError(w, &openai.Error{
		Status: http.StatusNotFound,
		Type:   openai.ErrNotFound,
		Code:   "model_not_found",
		Msg:    "model not found: " + model,
	})
}

// writeOpenAIErr renders an error as its OpenAI envelope. An *openai.Error
// carries its own status; an *anthropic.Error from a shared core path (e.g. the
// context-window assertion) is re-shaped onto the OpenAI envelope so the
// surface never leaks the Anthropic shape; engine-unavailable becomes a 529
// overloaded_error; anything else is a 500.
func writeOpenAIErr(w http.ResponseWriter, err error) {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		openai.WriteError(w, apiErr)
		return
	}
	var anthErr *anthropic.Error
	if errors.As(err, &anthErr) {
		openai.WriteError(w, &openai.Error{
			Status: anthErr.Status,
			Type:   openaiErrType(anthErr.Type),
			Msg:    anthErr.Msg,
		})
		return
	}
	if errors.Is(err, core.ErrEngineUnavailable) {
		openai.WriteError(w, &openai.Error{
			Status: statusOverloaded,
			Type:   openai.ErrOverloaded,
			Msg:    "the inference engine is unavailable; retry shortly",
		})
		return
	}
	openai.WriteError(w, &openai.Error{
		Status: http.StatusInternalServerError,
		Type:   openai.ErrAPI,
		Msg:    "internal error",
	})
}

// openaiErrType maps an Anthropic error-envelope type onto the OpenAI one (they
// share the same vocabulary in M0, so this is a direct re-label).
func openaiErrType(t anthropic.ErrorType) openai.ErrorType {
	switch t {
	case anthropic.ErrInvalidRequest:
		return openai.ErrInvalidRequest
	case anthropic.ErrAuthentication:
		return openai.ErrAuthentication
	case anthropic.ErrPermission:
		return openai.ErrPermission
	case anthropic.ErrNotFound:
		return openai.ErrNotFound
	case anthropic.ErrOverloaded:
		return openai.ErrOverloaded
	default:
		return openai.ErrAPI
	}
}

func newCompletionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}
