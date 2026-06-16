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

	truncated := text[:bestIdx]
	resp.Blocks = []ContentBlock{TextBlock(truncated)}
	resp.StopReason = StopStopSequence
	return resp, bestSeq
}
