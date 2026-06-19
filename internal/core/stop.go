package core

import "strings"

// ApplyStopSequences enforces Anthropic stop-sequence semantics on a response,
// engine-independently. The gateway calls this rather than relying on each
// engine's stop handling, so the stop_reason/stop_sequence a client sees is
// identical across llama.cpp, vLLM, and the rest.
//
// The earliest-occurring stop sequence in the concatenated text wins. Matched
// text and everything after it is removed, StopReason becomes StopStopSequence,
// and the matched sequence is returned so the gateway can populate the
// stop_sequence response field. If nothing matches, resp is unchanged and the
// returned string is empty.
//
// The match index is over the concatenated text blocks only (thinking and
// tool_use blocks are not part of the model's visible text). Blocks before the
// match are preserved as-is, the text block containing the match is truncated
// there, and everything after — including any later thinking/tool_use blocks —
// is dropped. This mirrors the streaming path, which ends the stream at the
// match and keeps the blocks it had already emitted; flattening the whole
// response to one text block here would drop a tool_use the model produced
// before the stop hit and make buffered and streamed responses diverge.
//
// It runs before any max_tokens accounting the engine already applied: a real
// stop hit means the model never reached the token budget.
func ApplyStopSequences(resp Response, stopSequences []string) (Response, string) {
	if len(stopSequences) == 0 {
		return resp, ""
	}

	text := resp.Text()
	bestIdx := -1
	var bestSeq string
	for _, seq := range stopSequences {
		if seq == "" {
			continue
		}
		if idx := strings.Index(text, seq); idx >= 0 && (bestIdx < 0 || idx < bestIdx) {
			bestIdx, bestSeq = idx, seq
		}
	}
	if bestIdx < 0 {
		return resp, ""
	}

	// Rebuild the block list, advancing a running offset over text-block content
	// until the block holding the match is reached.
	kept := make([]ContentBlock, 0, len(resp.Blocks))
	consumed := 0
	for _, b := range resp.Blocks {
		if b.Type != BlockText {
			kept = append(kept, b)
			continue
		}
		if end := consumed + len(b.Text); bestIdx >= end {
			kept = append(kept, b)
			consumed = end
			continue
		}
		if cut := bestIdx - consumed; cut > 0 {
			kept = append(kept, TextBlock(b.Text[:cut]))
		}
		break
	}
	// Guarantee a response always carries content, matching the prior behavior
	// when the match falls at the very start with no preceding blocks.
	if len(kept) == 0 {
		kept = append(kept, TextBlock(""))
	}
	resp.Blocks = kept
	resp.StopReason = StopStopSequence
	return resp, bestSeq
}
