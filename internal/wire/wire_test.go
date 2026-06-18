package wire_test

import (
	"encoding/json"
	"testing"

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
