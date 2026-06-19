// Package wire defines the WebSocket message types shared between the server
// hub and the worker client (ADR-0007). All messages are JSON text frames with
// the envelope {type, id?, payload?}. It carries two kinds of traffic: the
// control plane (join, heartbeat) and inference (execute/count_tokens/cancel
// server→worker; response/chunk/done/token_count/error worker→server). The
// inference payloads carry internal/core types — the same representation the
// in-process channel uses — so no translation layer is introduced. Drain lands
// in M1 phase 3.
package wire

import (
	"encoding/json"

	"github.com/orchestra-hq/atlas/internal/core"
)

// MessageType identifies the purpose of a WebSocket frame.
type MessageType string

// Worker↔server message types. Control-plane (join, heartbeat) plus the M1
// phase-2 inference set; drain is added in M1 phase 3.
const (
	MsgJoin         MessageType = "join"
	MsgJoinAck      MessageType = "join_ack"
	MsgHeartbeat    MessageType = "heartbeat"
	MsgHeartbeatAck MessageType = "heartbeat_ack"

	// Inference, server → worker.
	MsgExecute     MessageType = "execute"
	MsgCountTokens MessageType = "count_tokens"
	MsgCancel      MessageType = "cancel"

	// Inference, worker → server.
	MsgResponse   MessageType = "response"    // reply to a non-streaming execute
	MsgChunk      MessageType = "chunk"       // one streamed delta
	MsgDone       MessageType = "done"        // end of a streamed execute
	MsgTokenCount MessageType = "token_count" // reply to count_tokens
	MsgError      MessageType = "error"       // a request failed
)

// Error codes carried in ErrorPayload.Code, so the gateway can reconstruct the
// right client-facing status (notably mapping an unavailable engine to a
// retryable 529) from a worker-reported failure.
const (
	CodeEngineUnavailable = "engine_unavailable"
	CodeInternal          = "internal"
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
	// Models are the model instances this worker already serves and will accept
	// execute messages for. In M1 phase 2 the worker self-declares them (atlas
	// worker --model); the phase-4 scheduler drives placement instead.
	Models []ServedModel `json:"models,omitempty"`
}

// ServedModel advertises one model a worker serves: the name clients address
// and its context window in tokens (0 = unknown, the gateway skips its fit
// assertion). The gateway registers a route per ServedModel on join.
type ServedModel struct {
	Name          string `json:"name"`
	ContextWindow int    `json:"context_window,omitempty"`
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

// ExecutePayload runs one inference request on the worker (server → worker).
// Stream selects the worker's streaming path (chunk/done) over the buffered one
// (response). The Request carries no stop sequences: the gateway owns
// stop-sequence semantics and strips them before dispatch.
type ExecutePayload struct {
	RequestID string       `json:"request_id"`
	Stream    bool         `json:"stream,omitempty"`
	Request   core.Request `json:"request"`
}

// CountTokensPayload asks the worker to count a request's prompt tokens with
// the engine's real tokenizer (server → worker); answered by token_count.
type CountTokensPayload struct {
	RequestID string       `json:"request_id"`
	Request   core.Request `json:"request"`
}

// CancelPayload tells the worker to stop an in-flight request (server → worker),
// sent when the client disconnects or a stop sequence matches mid-stream.
type CancelPayload struct {
	RequestID string `json:"request_id"`
}

// ResponsePayload is the buffered result of a non-streaming execute
// (worker → server).
type ResponsePayload struct {
	RequestID string        `json:"request_id"`
	Response  core.Response `json:"response"`
}

// ChunkPayload carries one streamed delta of a streaming execute
// (worker → server). Chunks for a request arrive in order.
type ChunkPayload struct {
	RequestID string           `json:"request_id"`
	Event     core.StreamEvent `json:"event"`
}

// DonePayload ends a streaming execute with the engine's stop reason and final
// usage (worker → server).
type DonePayload struct {
	RequestID  string          `json:"request_id"`
	StopReason core.StopReason `json:"stop_reason"`
	Usage      core.Usage      `json:"usage"`
}

// TokenCountPayload answers a count_tokens request (worker → server).
type TokenCountPayload struct {
	RequestID string `json:"request_id"`
	Count     int    `json:"count"`
}

// ErrorPayload reports that a request failed (worker → server). Code is one of
// the Code* constants; Message is for logging, not the client.
type ErrorPayload struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// Encode marshals payload into a Message with the given type and optional id.
func Encode(typ MessageType, id string, payload any) (Message, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	return Message{Type: typ, ID: id, Payload: json.RawMessage(data)}, nil
}
