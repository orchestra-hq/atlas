// Package core holds Atlas's internal representation of conversations:
// messages, content blocks, and stop reasons. The wire formats under
// internal/api translate to and from these types so engine adapters and the
// gateway never depend on a vendor's JSON shape. Tools and thinking blocks
// arrive in build-plan phases 4–5.
package core

// Role identifies who produced a message in a conversation.
type Role string

// Conversation roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// StopReason says why generation ended, in Anthropic vocabulary. Engine
// adapters map their engine's vocabulary onto these values; the gateway may
// rewrite EndTurn into StopSequence when it applies stop sequences itself.
type StopReason string

// Stop reasons, in Anthropic vocabulary.
const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopToolUse      StopReason = "tool_use"
)

// BlockType discriminates content blocks. M0 phase 2 is text-only; tool_use,
// tool_result, and thinking arrive in build-plan phases 4–5.
type BlockType string

// Content block types. M0 phase 2 is text-only.
const (
	BlockText BlockType = "text"
)

// ContentBlock is one unit of message content. A flat struct rather than an
// interface: later block kinds (tool_use, thinking) add fields, and adapters
// switch on Type.
type ContentBlock struct {
	Type BlockType
	Text string
}

// TextBlock builds a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// Message is one conversation turn.
type Message struct {
	Role   Role
	Blocks []ContentBlock
}

// Text concatenates the text of every text block in the message.
func (m Message) Text() string {
	var s string
	for _, b := range m.Blocks {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}

// Request is the engine-independent inference request. Wire handlers
// (internal/api) produce it; engine adapters (internal/engines) consume it.
//
// StopSequences are carried for adapters that can push them down to the
// engine, but the gateway owns Anthropic stop-sequence semantics (matching,
// truncation, the stop_sequence response field) so behavior is identical
// across engines.
type Request struct {
	Model         string
	System        string
	Messages      []Message
	MaxTokens     int
	Temperature   *float64
	TopP          *float64
	TopK          *int
	StopSequences []string
}

// Usage is token accounting as reported by the engine.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is the engine-independent inference result. ID, model echo, and
// stop_sequence are wire concerns the gateway adds when encoding.
type Response struct {
	Blocks     []ContentBlock
	StopReason StopReason
	Usage      Usage
}

// Text concatenates the text of every text block in the response.
func (r Response) Text() string {
	var s string
	for _, b := range r.Blocks {
		if b.Type == BlockText {
			s += b.Text
		}
	}
	return s
}
