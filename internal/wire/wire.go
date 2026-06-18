// Package wire defines the WebSocket message types shared between the server
// hub and the worker client (ADR-0007). All messages are JSON text frames with
// the envelope {type, id?, payload?}. Only the control-plane message types
// (join, heartbeat) are defined here; inference types (execute, chunk, done,
// error, drain) land in M1 phase 2.
package wire

import "encoding/json"

// MessageType identifies the purpose of a WebSocket frame.
type MessageType string

// Control-plane message types for the worker↔server WebSocket channel.
// Inference message types (execute, chunk, done, error, drain) are added in
// M1 phase 2.
const (
	MsgJoin         MessageType = "join"
	MsgJoinAck      MessageType = "join_ack"
	MsgHeartbeat    MessageType = "heartbeat"
	MsgHeartbeatAck MessageType = "heartbeat_ack"
)

// Message is the outer envelope for every worker↔server WebSocket frame.
type Message struct {
	Type    MessageType     `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hardware describes a worker's compute resources, reported in JoinPayload.
// GPU detection is minimal in phase 1 and expanded in M1 phase 4 once the
// scheduler needs accurate VRAM figures.
type Hardware struct {
	Platform string `json:"platform"` // "cuda", "metal", or "cpu"
	RAMBytes int64  `json:"ram_bytes"`
	GPUs     []GPU  `json:"gpus,omitempty"`
}

// GPU is one accelerator in a Hardware inventory.
type GPU struct {
	Name      string `json:"name"`
	VRAMBytes int64  `json:"vram_bytes"`
}

// JoinPayload is the payload of a join message (worker → server).
type JoinPayload struct {
	Token    string   `json:"token"`
	Hardware Hardware `json:"hardware"`
	Version  string   `json:"version"`
	Name     string   `json:"name,omitempty"`
}

// JoinAckPayload is the server's response to a join message.
type JoinAckPayload struct {
	Accepted bool   `json:"accepted"`
	WorkerID string `json:"worker_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// HeartbeatPayload is sent by the worker every 10 s to keep the connection
// alive and allow the server to detect dropped workers (phase 3).
type HeartbeatPayload struct {
	WorkerID string `json:"worker_id"`
}

// HeartbeatAckPayload is the server's response to a heartbeat.
type HeartbeatAckPayload struct{}

// Encode marshals payload into a Message with the given type and optional id.
func Encode(typ MessageType, id string, payload any) (Message, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	return Message{Type: typ, ID: id, Payload: json.RawMessage(data)}, nil
}
