package openaichat

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

// Client drives an engine's OpenAI-compatible /v1/chat/completions endpoint. An
// engine adapter embeds it for generation and adds its own token-counting and
// context-window endpoints (which differ per engine). name prefixes errors so
// the failing engine is identifiable.
type Client struct {
	name    string
	baseURL string
	model   string
	http    *http.Client
}

// NewClient builds a client for engine name targeting baseURL (e.g.
// http://127.0.0.1:8000). model is the name echoed in the OpenAI payload; the
// engine serves whatever weights it was launched with regardless.
func NewClient(name, baseURL, model string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{name: name, baseURL: baseURL, model: model, http: hc}
}

// Model returns the served model name the client echoes to the engine.
func (c *Client) Model() string { return c.model }

// Execute translates req to OpenAI chat form, calls the engine, and maps the
// result back to core.
func (c *Client) Execute(ctx context.Context, req core.Request) (core.Response, error) {
	body, err := json.Marshal(BuildRequest(c.model, req, false))
	if err != nil {
		return core.Response{}, fmt.Errorf("%s: encode request: %w", c.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return core.Response{}, fmt.Errorf("%s: build request: %w", c.name, err)
	}
	httpReq.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return core.Response{}, fmt.Errorf("%s: call engine: %w: %w", c.name, core.ErrEngineUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Response{}, fmt.Errorf("%s: read response: %w: %w", c.name, core.ErrEngineUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return core.Response{}, fmt.Errorf("%s: engine returned %d: %s: %w", c.name, resp.StatusCode, truncate(raw, 512), core.ErrEngineUnavailable)
	}

	var chat Response
	if err := json.Unmarshal(raw, &chat); err != nil {
		return core.Response{}, fmt.Errorf("%s: decode response: %w", c.name, err)
	}
	if len(chat.Choices) == 0 {
		return core.Response{}, fmt.Errorf("%s: engine returned no choices", c.name)
	}
	return ParseResponse(req, chat), nil
}

// ExecuteStream runs req with stream=true, forwarding each delta to sink and
// ending with sink.Done. If a sink method returns core.ErrStopStreaming (the
// gateway's signal that a stop sequence matched), generation is abandoned and
// ExecuteStream returns nil — the stream is finalized by the gateway, not here.
func (c *Client) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	body, err := json.Marshal(BuildRequest(c.model, req, true))
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", c.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", c.name, err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: call engine: %w: %w", c.name, core.ErrEngineUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: engine returned %d: %s: %w", c.name, resp.StatusCode, truncate(raw, 512), core.ErrEngineUnavailable)
	}

	reason := core.StopEndTurn
	var usage core.Usage
	sawToolCall := false
	wantThinking := EmitThinking(req)
	started := map[int]bool{} // tool-call indices already announced via ToolCallStart
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

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("%s: decode stream chunk: %w", c.name, err)
		}
		if chunk.Usage != nil {
			usage = core.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) == 0 {
			continue // usage-only final chunk
		}
		choice := chunk.Choices[0]
		if wantThinking && choice.Delta.ReasoningContent != "" {
			if stop, err := pump(sink.Thinking(choice.Delta.ReasoningContent)); err != nil {
				return err
			} else if stop {
				return nil
			}
		}
		if choice.Delta.Content != "" {
			if stop, err := pump(sink.Text(choice.Delta.Content)); err != nil {
				return err
			} else if stop {
				return nil
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			sawToolCall = true
			if !started[tc.Index] {
				started[tc.Index] = true
				if stop, err := pump(sink.ToolCallStart(tc.Index, toolUseID(tc.ID, tc.Index), tc.Function.Name)); err != nil {
					return err
				} else if stop {
					return nil
				}
			}
			if tc.Function.Arguments != "" {
				if stop, err := pump(sink.ToolCallDelta(tc.Index, tc.Function.Arguments)); err != nil {
					return err
				} else if stop {
					return nil
				}
			}
		}
		if choice.FinishReason != nil {
			reason = MapFinishReason(*choice.FinishReason)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: read stream: %w", c.name, err)
	}
	if sawToolCall {
		reason = core.StopToolUse
	}

	if _, err := pump(sink.Done(reason, usage)); err != nil {
		return err
	}
	return nil
}

// pump interprets the error from a StreamSink call: ErrStopStreaming means the
// gateway wants generation to end cleanly (stop=true, no error); any other
// non-nil error propagates.
func pump(err error) (stop bool, _ error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, core.ErrStopStreaming):
		return true, nil
	default:
		return false, err
	}
}

// PostJSON marshals body to path, decodes a 200 response into out (skipped when
// nil), and wraps transport failures and non-200 statuses with
// core.ErrEngineUnavailable so the gateway maps them to a 529. Engine adapters
// use it for their token-counting and context-window endpoints.
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode %s request: %w", c.name, path, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: build %s request: %w", c.name, path, err)
	}
	httpReq.Header.Set("content-type", "application/json")
	return c.doJSON(httpReq, path, out)
}

// GetJSON issues a GET to path and decodes a 200 response into out.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%s: build %s request: %w", c.name, path, err)
	}
	return c.doJSON(httpReq, path, out)
}

func (c *Client) doJSON(httpReq *http.Request, path string, out any) error {
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: call %s: %w: %w", c.name, path, core.ErrEngineUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read %s response: %w: %w", c.name, path, core.ErrEngineUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s returned %d: %s: %w", c.name, path, resp.StatusCode, truncate(raw, 512), core.ErrEngineUnavailable)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s: decode %s response: %w", c.name, path, err)
		}
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
