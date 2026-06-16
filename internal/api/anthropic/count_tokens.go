package anthropic

import "github.com/orchestra-hq/atlas/internal/core"

// CountTokensRequest is the POST /v1/messages/count_tokens wire request. It is
// a Messages request without generation parameters: agents send the same
// model/system/messages/tools they intend to generate with, and Atlas returns
// the prompt's token count so they can budget against the model's window.
type CountTokensRequest struct {
	Model      string          `json:"model"`
	System     StringOrBlocks  `json:"system"`
	Messages   []WireMessage   `json:"messages"`
	Tools      []WireTool      `json:"tools"`
	ToolChoice *WireToolChoice `json:"tool_choice"`
	Thinking   *WireThinking   `json:"thinking"`
}

// ToCore validates and translates the request into a core.Request. It reuses
// the Messages validation and translation so a count request and the identical
// generation request map to the same prompt — the count must match that
// request's usage.input_tokens. MaxTokens is irrelevant to counting and is left
// zero; a sentinel satisfies the shared max_tokens validation first.
func (r *CountTokensRequest) ToCore() (core.Request, error) {
	mr := MessagesRequest{
		Model:      r.Model,
		System:     r.System,
		Messages:   r.Messages,
		MaxTokens:  1, // count_tokens carries no max_tokens; satisfy shared validation
		Tools:      r.Tools,
		ToolChoice: r.ToolChoice,
		Thinking:   r.Thinking,
	}
	req, err := mr.ToCore()
	if err != nil {
		return core.Request{}, err
	}
	req.MaxTokens = 0
	return req, nil
}

// CountTokensResponse is the POST /v1/messages/count_tokens wire response.
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}
