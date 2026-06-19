package core

import (
	"encoding/json"
	"testing"
)

func resp(text string) Response {
	return Response{Blocks: []ContentBlock{TextBlock(text)}, StopReason: StopEndTurn}
}

func TestApplyStopSequences(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		stops    []string
		wantText string
		wantSeq  string
		wantStop StopReason
	}{
		{"no stops", "one two three", nil, "one two three", "", StopEndTurn},
		{"no match", "one two three", []string{"zzz"}, "one two three", "", StopEndTurn},
		{"single match truncates", "one two three four", []string{"three"}, "one two ", "three", StopStopSequence},
		{"earliest of several wins", "alpha beta gamma", []string{"gamma", "beta"}, "alpha ", "beta", StopStopSequence},
		{"empty sequences ignored", "one two", []string{"", "two"}, "one ", "two", StopStopSequence},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, seq := ApplyStopSequences(resp(tc.text), tc.stops)
			if got.Text() != tc.wantText {
				t.Errorf("text = %q, want %q", got.Text(), tc.wantText)
			}
			if seq != tc.wantSeq {
				t.Errorf("seq = %q, want %q", seq, tc.wantSeq)
			}
			if got.StopReason != tc.wantStop {
				t.Errorf("stop_reason = %q, want %q", got.StopReason, tc.wantStop)
			}
		})
	}
}

// A stop hit in trailing text must preserve earlier non-text blocks (a tool_use
// or thinking the model already produced), not flatten the whole response to one
// text block — otherwise the buffered path drops a tool call the streaming path
// would have kept, breaking the agent loop.
func TestApplyStopSequencesPreservesEarlierBlocks(t *testing.T) {
	in := Response{
		Blocks: []ContentBlock{
			ThinkingBlock("reasoning", ""),
			ToolUseBlock("t1", "search", json.RawMessage(`{"q":"x"}`)),
			TextBlock("answer <END> tail"),
		},
		StopReason: StopToolUse,
	}
	got, seq := ApplyStopSequences(in, []string{"<END>"})

	if seq != "<END>" {
		t.Fatalf("seq = %q, want %q", seq, "<END>")
	}
	if got.StopReason != StopStopSequence {
		t.Errorf("stop_reason = %q, want %q", got.StopReason, StopStopSequence)
	}
	want := []BlockType{BlockThinking, BlockToolUse, BlockText}
	if len(got.Blocks) != len(want) {
		t.Fatalf("kept %d blocks, want %d: %+v", len(got.Blocks), len(want), got.Blocks)
	}
	for i, wt := range want {
		if got.Blocks[i].Type != wt {
			t.Errorf("block %d type = %q, want %q", i, got.Blocks[i].Type, wt)
		}
	}
	if got.Blocks[1].Name != "search" {
		t.Errorf("tool_use block not preserved: %+v", got.Blocks[1])
	}
	if got.Text() != "answer " {
		t.Errorf("text = %q, want %q", got.Text(), "answer ")
	}
}

// A match that lands beyond a text block keeps that block intact and drops only
// the blocks after the match point.
func TestApplyStopSequencesDropsBlocksAfterMatch(t *testing.T) {
	in := Response{
		Blocks: []ContentBlock{
			TextBlock("alpha "),
			TextBlock("beta STOP gamma"),
			ToolUseBlock("t1", "search", json.RawMessage(`{}`)),
		},
	}
	got, seq := ApplyStopSequences(in, []string{"STOP"})

	if seq != "STOP" {
		t.Fatalf("seq = %q, want %q", seq, "STOP")
	}
	if got.Text() != "alpha beta " {
		t.Errorf("text = %q, want %q", got.Text(), "alpha beta ")
	}
	for _, b := range got.Blocks {
		if b.Type == BlockToolUse {
			t.Errorf("tool_use after the match should have been dropped: %+v", got.Blocks)
		}
	}
}
