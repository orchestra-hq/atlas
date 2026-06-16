// Package llamacpp adapts llama.cpp's bundled HTTP server (llama-server) to
// Atlas's engine interface. Per build-time decision 1 in
// docs/m0-build-plan.md, the adapter speaks llama-server's OpenAI-compatible
// /v1/chat/completions endpoint; the gateway produces all Anthropic
// semantics, so one conformance result holds across engines.
package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/orchestra-hq/atlas/internal/core"
)

// Adapter executes core requests against a running llama-server instance.
type Adapter struct {
	baseURL string
	model   string
	client  *http.Client
}

// New builds an adapter targeting a llama-server at baseURL (e.g.
// http://127.0.0.1:8080). model is the name echoed in the OpenAI payload;
// llama-server serves whatever weights it was launched with regardless.
func New(baseURL, model string, client *http.Client) *Adapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &Adapter{baseURL: baseURL, model: model, client: client}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	MaxTokens     int            `json:"max_tokens"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	TopK          *int           `json:"top_k,omitempty"` // llama.cpp extension, ignored by strict OpenAI servers
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions asks the OpenAI-compat server to include a final usage chunk
// when streaming (llama-server honors this).
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Execute translates req to OpenAI chat form, calls llama-server, and maps the
// result back to core. Stop sequences are intentionally not forwarded: the
// gateway owns that semantics (see server.Gateway).
func (a *Adapter) Execute(ctx context.Context, req core.Request) (core.Response, error) {
	body, err := json.Marshal(a.toChat(req, false))
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: call engine: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return core.Response{}, fmt.Errorf("llamacpp: engine returned %d: %s", resp.StatusCode, truncate(raw, 512))
	}

	var chat chatResponse
	if err := json.Unmarshal(raw, &chat); err != nil {
		return core.Response{}, fmt.Errorf("llamacpp: decode response: %w", err)
	}
	if len(chat.Choices) == 0 {
		return core.Response{}, fmt.Errorf("llamacpp: engine returned no choices")
	}

	choice := chat.Choices[0]
	return core.Response{
		Blocks:     []core.ContentBlock{core.TextBlock(choice.Message.Content)},
		StopReason: mapFinishReason(choice.FinishReason),
		Usage: core.Usage{
			InputTokens:  chat.Usage.PromptTokens,
			OutputTokens: chat.Usage.CompletionTokens,
		},
	}, nil
}

// chatStreamChunk is one Server-Sent Event from the OpenAI-compat streaming
// endpoint. Deltas carry incremental content; the terminal chunk carries a
// finish_reason and (with stream_options.include_usage) a usage object.
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ExecuteStream runs req against llama-server with stream=true, forwarding each
// content delta to sink.Text and ending with sink.Done. If a sink method
// returns core.ErrStopStreaming (the gateway's signal that a stop sequence
// matched), generation is abandoned and ExecuteStream returns nil — the stream
// is finalized by the gateway, not here. As with Execute, stop sequences are
// owned by the gateway and never forwarded to the engine.
func (a *Adapter) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	body, err := json.Marshal(a.toChat(req, true))
	if err != nil {
		return fmt.Errorf("llamacpp: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llamacpp: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("llamacpp: call engine: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("llamacpp: engine returned %d: %s", resp.StatusCode, truncate(raw, 512))
	}

	reason := core.StopEndTurn
	var usage core.Usage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // event:/id:/comment/blank lines
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}

		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("llamacpp: decode stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			usage = core.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) == 0 {
			continue // usage-only final chunk
		}
		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			if err := sink.Text(choice.Delta.Content); err != nil {
				if errors.Is(err, core.ErrStopStreaming) {
					return nil
				}
				return err
			}
		}
		if choice.FinishReason != nil {
			reason = mapFinishReason(*choice.FinishReason)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("llamacpp: read stream: %w", err)
	}

	if err := sink.Done(reason, usage); err != nil {
		if errors.Is(err, core.ErrStopStreaming) {
			return nil
		}
		return err
	}
	return nil
}

func (a *Adapter) toChat(req core.Request, stream bool) chatRequest {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: string(m.Role), Content: m.Text()})
	}
	cr := chatRequest{
		Model:       a.model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		TopK:        req.TopK,
		Stream:      stream,
	}
	if stream {
		cr.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return cr
}

// mapFinishReason maps OpenAI finish_reason onto Anthropic stop reasons. Tool
// calls (tool_calls) arrive in build-plan phase 4. An empty/unknown reason is
// treated as a normal end of turn.
func mapFinishReason(reason string) core.StopReason {
	switch reason {
	case "length":
		return core.StopMaxTokens
	case "stop", "":
		return core.StopEndTurn
	default:
		return core.StopEndTurn
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
