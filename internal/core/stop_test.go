package core

import "testing"

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
