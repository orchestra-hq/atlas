// Package core holds Atlas's internal representation of conversations:
// messages, content blocks, and stop reasons. The wire formats under
// internal/api translate to and from these types so engine adapters and the
// gateway never depend on a vendor's JSON shape.
package core

import "encoding/json"

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

// BlockType discriminates content blocks.
type BlockType string

// Content block types.
const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// ContentBlock is one unit of message content. A flat struct rather than an
// interface: each block kind uses the subset of fields relevant to its Type,
// and adapters switch on Type.
type ContentBlock struct {
	Type BlockType
	Text string // BlockText

	// BlockToolUse: the assistant calls a tool. Input is the arguments as a
	// JSON object (raw, so it round-trips byte-for-byte to the engine).
	ID    string
	Name  string
	Input json.RawMessage

	// BlockToolResult: a user turn returns a tool's output. ToolUseID matches
	// the ID of the BlockToolUse it answers; Content is the result flattened to
	// text (M0 engines are text-only); IsError marks a failed call.
	ToolUseID string
	Content   string
	IsError   bool

	// BlockThinking: the model's reasoning trace, mapped from an engine's
	// reasoning output (ADR-0005). Thinking is the reasoning text. Signature is
	// accepted on echoed-back input but never produced — Atlas does not emulate
	// provider-side signing.
	Thinking  string
	Signature string
}

// TextBlock builds a text content block.
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: BlockText, Text: text}
}

// ToolUseBlock builds an assistant tool_use block. input is the tool arguments
// as a JSON object.
func ToolUseBlock(id, name string, input json.RawMessage) ContentBlock {
	return ContentBlock{Type: BlockToolUse, ID: id, Name: name, Input: input}
}

// ToolResultBlock builds a user tool_result block answering the tool_use with
// the given ID.
func ToolResultBlock(toolUseID, content string, isError bool) ContentBlock {
	return ContentBlock{Type: BlockToolResult, ToolUseID: toolUseID, Content: content, IsError: isError}
}

// ThinkingBlock builds a thinking content block. signature is carried for
// round-tripping echoed-back input but is empty on blocks Atlas produces.
func ThinkingBlock(thinking, signature string) ContentBlock {
	return ContentBlock{Type: BlockThinking, Thinking: thinking, Signature: signature}
}

// Tool is a tool the model may call. InputSchema is a JSON Schema object,
// carried raw and passed to the engine unchanged.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolChoiceType says how the model must use the available tools, in Anthropic
// vocabulary. Adapters map it onto their engine's equivalent.
type ToolChoiceType string

// Tool-choice modes.
const (
	ToolChoiceAuto ToolChoiceType = "auto" // model decides whether to call a tool
	ToolChoiceAny  ToolChoiceType = "any"  // model must call some tool
	ToolChoiceTool ToolChoiceType = "tool" // model must call the named tool
	ToolChoiceNone ToolChoiceType = "none" // model must not call a tool
)

// ToolChoice constrains tool use. Name is set only when Type is ToolChoiceTool.
type ToolChoice struct {
	Type ToolChoiceType
	Name string
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

	// Tools the model may call, and how it must choose among them. ToolChoice
	// is nil when the request sets no constraint (engine default: auto).
	Tools      []Tool
	ToolChoice *ToolChoice

	// Thinking requests reasoning output (ADR-0005). Nil means the client did
	// not ask for thinking; adapters then suppress reasoning where the engine
	// would otherwise emit it. Non-nil with Enabled maps reasoning output to
	// thinking blocks.
	Thinking *ThinkingConfig
}

// ThinkingConfig is the request's thinking setting. BudgetTokens is advisory
// (ADR-0005): adapters map it to the engine's reasoning-effort knob where one
// exists and otherwise ignore it — Atlas never truncates reasoning to enforce
// a budget.
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
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
