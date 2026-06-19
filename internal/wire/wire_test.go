package wire_test

import (
	"encoding/json"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/wire"
)

func TestEncode_roundtrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     wire.MessageType
		payload any
		decode  func(json.RawMessage) error
	}{
		{
			name: "join",
			typ:  wire.MsgJoin,
			payload: wire.JoinPayload{
				Token:   "tok",
				Version: "v1",
				Name:    "box",
				Hardware: wire.Hardware{
					Platform: "cuda",
					RAMBytes: 64 << 30,
					GPUs:     []wire.GPU{{Name: "A100", VRAMBytes: 80 << 30}},
				},
			},
			decode: func(raw json.RawMessage) error {
				var p wire.JoinPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.Token != "tok" || p.Hardware.Platform != "cuda" || len(p.Hardware.GPUs) != 1 {
					t.Errorf("decoded join: %+v", p)
				}
				return nil
			},
		},
		{
			name:    "join_ack_accepted",
			typ:     wire.MsgJoinAck,
			payload: wire.JoinAckPayload{Accepted: true, WorkerID: "w_abc"},
			decode: func(raw json.RawMessage) error {
				var p wire.JoinAckPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if !p.Accepted || p.WorkerID != "w_abc" {
					t.Errorf("decoded join_ack: %+v", p)
				}
				return nil
			},
		},
		{
			name:    "join_ack_rejected",
			typ:     wire.MsgJoinAck,
			payload: wire.JoinAckPayload{Accepted: false, Reason: "invalid token"},
			decode: func(raw json.RawMessage) error {
				var p wire.JoinAckPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.Accepted || p.Reason != "invalid token" {
					t.Errorf("decoded join_ack: %+v", p)
				}
				return nil
			},
		},
		{
			name:    "heartbeat",
			typ:     wire.MsgHeartbeat,
			payload: wire.HeartbeatPayload{WorkerID: "w_abc"},
			decode: func(raw json.RawMessage) error {
				var p wire.HeartbeatPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.WorkerID != "w_abc" {
					t.Errorf("decoded heartbeat: %+v", p)
				}
				return nil
			},
		},
		{
			name:    "heartbeat_ack",
			typ:     wire.MsgHeartbeatAck,
			payload: wire.HeartbeatAckPayload{},
			decode: func(raw json.RawMessage) error {
				var p wire.HeartbeatAckPayload
				return json.Unmarshal(raw, &p)
			},
		},
		{
			name: "join_with_models",
			typ:  wire.MsgJoin,
			payload: wire.JoinPayload{
				Token:  "tok",
				Models: []wire.ServedModel{{Name: "qwen2.5", ContextWindow: 4096}},
			},
			decode: func(raw json.RawMessage) error {
				var p wire.JoinPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if len(p.Models) != 1 || p.Models[0].Name != "qwen2.5" || p.Models[0].ContextWindow != 4096 {
					t.Errorf("decoded join models: %+v", p.Models)
				}
				return nil
			},
		},
		{
			name: "execute",
			typ:  wire.MsgExecute,
			payload: wire.ExecutePayload{
				RequestID: "req-1",
				Stream:    true,
				Request:   core.Request{Model: "qwen2.5", MaxTokens: 64, Messages: []core.Message{{Role: core.RoleUser, Blocks: []core.ContentBlock{core.TextBlock("hi")}}}},
			},
			decode: func(raw json.RawMessage) error {
				var p wire.ExecutePayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.RequestID != "req-1" || !p.Stream || p.Request.Model != "qwen2.5" || p.Request.Messages[0].Text() != "hi" {
					t.Errorf("decoded execute: %+v", p)
				}
				return nil
			},
		},
		{
			name: "chunk",
			typ:  wire.MsgChunk,
			payload: wire.ChunkPayload{
				RequestID: "req-1",
				Event:     core.StreamEvent{Kind: core.EventToolStart, Index: 0, ID: "tu_1", Name: "get_weather"},
			},
			decode: func(raw json.RawMessage) error {
				var p wire.ChunkPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.Event.Kind != core.EventToolStart || p.Event.Name != "get_weather" {
					t.Errorf("decoded chunk: %+v", p)
				}
				return nil
			},
		},
		{
			name:    "done",
			typ:     wire.MsgDone,
			payload: wire.DonePayload{RequestID: "req-1", StopReason: core.StopEndTurn, Usage: core.Usage{InputTokens: 3, OutputTokens: 5}},
			decode: func(raw json.RawMessage) error {
				var p wire.DonePayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.StopReason != core.StopEndTurn || p.Usage.OutputTokens != 5 {
					t.Errorf("decoded done: %+v", p)
				}
				return nil
			},
		},
		{
			name:    "error",
			typ:     wire.MsgError,
			payload: wire.ErrorPayload{RequestID: "req-1", Code: wire.CodeEngineUnavailable, Message: "engine down"},
			decode: func(raw json.RawMessage) error {
				var p wire.ErrorPayload
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if p.Code != wire.CodeEngineUnavailable {
					t.Errorf("decoded error: %+v", p)
				}
				return nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := wire.Encode(tc.typ, "id-1", tc.payload)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if msg.Type != tc.typ {
				t.Errorf("type: got %q want %q", msg.Type, tc.typ)
			}

			// Re-encode to JSON and decode back to verify round-trip.
			data, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded wire.Message
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded.Type != tc.typ {
				t.Errorf("decoded type: got %q want %q", decoded.Type, tc.typ)
			}
			if err := tc.decode(decoded.Payload); err != nil {
				t.Errorf("payload decode: %v", err)
			}
		})
	}
}
