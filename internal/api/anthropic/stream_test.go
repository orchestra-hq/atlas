package anthropic

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// sseEvent is one parsed event: name + decoded data object.
type sseEvent struct {
	name string
	data map[string]any
}

// parseSSE splits an event-stream body into ordered events.
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("event %q has invalid JSON data %q: %v", name, data, err)
		}
		events = append(events, sseEvent{name: name, data: obj})
	}
	return events
}

func names(events []sseEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.name
	}
	return out
}

func TestStreamWriterFullSequence(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := NewStreamWriter(rec, "msg_test", "demo-model")
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	if err := sw.Start(7); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, chunk := range []string{"Hello", ", ", "world"} {
		if err := sw.TextDelta(chunk); err != nil {
			t.Fatalf("TextDelta: %v", err)
		}
	}
	if err := sw.Finish(core.StopEndTurn, nil, core.Usage{InputTokens: 7, OutputTokens: 3}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	events := parseSSE(t, rec.Body.String())
	got := names(events)
	want := []string{
		"message_start", "content_block_start",
		"content_block_delta", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event names = %v, want %v", got, want)
	}

	// message_start carries the id/model and a zeroed output count.
	msg := events[0].data["message"].(map[string]any)
	if msg["id"] != "msg_test" || msg["model"] != "demo-model" {
		t.Errorf("message_start message = %v", msg)
	}

	// Deltas concatenate to the full text.
	var text strings.Builder
	for _, e := range events {
		if e.name == "content_block_delta" {
			text.WriteString(e.data["delta"].(map[string]any)["text"].(string))
		}
	}
	if text.String() != "Hello, world" {
		t.Errorf("concatenated text = %q", text.String())
	}

	// message_delta carries the stop reason and final usage.
	var md map[string]any
	for _, e := range events {
		if e.name == "message_delta" {
			md = e.data
		}
	}
	delta := md["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", delta["stop_reason"])
	}
	if delta["stop_sequence"] != nil {
		t.Errorf("stop_sequence = %v, want null", delta["stop_sequence"])
	}
	if usage := md["usage"].(map[string]any); usage["output_tokens"].(float64) != 3 {
		t.Errorf("output_tokens = %v, want 3", usage["output_tokens"])
	}
}

func TestStreamWriterStopSequence(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewStreamWriter(rec, "msg_x", "m")
	_ = sw.Start(0)
	_ = sw.TextDelta("kept")
	seq := "STOP"
	_ = sw.Finish(core.StopStopSequence, &seq, core.Usage{})

	events := parseSSE(t, rec.Body.String())
	var md map[string]any
	for _, e := range events {
		if e.name == "message_delta" {
			md = e.data
		}
	}
	delta := md["delta"].(map[string]any)
	if delta["stop_reason"] != "stop_sequence" || delta["stop_sequence"] != "STOP" {
		t.Errorf("delta = %v, want stop_sequence/STOP", delta)
	}
}

func TestStreamWriterToolUse(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewStreamWriter(rec, "msg_t", "m")
	_ = sw.Start(0)
	_ = sw.TextDelta("Let me check.")
	_ = sw.ToolUseStart("call_1", "get_weather")
	_ = sw.ToolUseDelta(`{"city":`)
	_ = sw.ToolUseDelta(`"Paris"}`)
	_ = sw.Finish(core.StopToolUse, nil, core.Usage{InputTokens: 5, OutputTokens: 9})

	events := parseSSE(t, rec.Body.String())
	got := names(events)
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop", // text, index 0
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // tool_use, index 1
		"message_delta", "message_stop",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event names = %v, want %v", got, want)
	}

	// The text block is index 0; the tool_use block is index 1 and carries id+name.
	var toolStart sseEvent
	for _, e := range events {
		if e.name == "content_block_start" && e.data["index"].(float64) == 1 {
			toolStart = e
		}
	}
	block := toolStart.data["content_block"].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "get_weather" {
		t.Errorf("tool_use start block = %v", block)
	}

	// input_json_delta fragments concatenate to valid JSON arguments.
	var args strings.Builder
	for _, e := range events {
		if e.name == "content_block_delta" {
			d := e.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				args.WriteString(d["partial_json"].(string))
			}
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("concatenated args %q not valid JSON: %v", args.String(), err)
	}
	if parsed["city"] != "Paris" {
		t.Errorf("args = %v", parsed)
	}

	if md := lastMessageDelta(events); md["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", md["stop_reason"])
	}
}

// lastMessageDelta returns the delta object of the final message_delta event.
func lastMessageDelta(events []sseEvent) map[string]any {
	var md map[string]any
	for _, e := range events {
		if e.name == "message_delta" {
			md = e.data["delta"].(map[string]any)
		}
	}
	return md
}

func TestStreamWriterDropsEmptyDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, _ := NewStreamWriter(rec, "id", "m")
	_ = sw.Start(0)
	if err := sw.TextDelta(""); err != nil {
		t.Fatalf("TextDelta(\"\"): %v", err)
	}
	for _, e := range parseSSE(t, rec.Body.String()) {
		if e.name == "content_block_delta" {
			t.Fatal("empty delta should not emit an event")
		}
	}
}
