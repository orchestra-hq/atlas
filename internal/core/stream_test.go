package core

import (
	"strings"
	"testing"
)

// drive feeds chunks through a scanner and returns the concatenated emitted
// text, whether a stop sequence matched, and the matched sequence. It mirrors
// how the gateway drives the scanner: Push per chunk, Flush at clean end.
func drive(seqs []string, chunks []string) (emitted string, matched bool, seq string) {
	s := NewStopSequenceScanner(seqs)
	var b strings.Builder
	for _, c := range chunks {
		out, hit := s.Push(c)
		b.WriteString(out)
		if hit {
			matched = true
			seq = s.Matched()
			break
		}
	}
	if !matched {
		b.WriteString(s.Flush())
	}
	return b.String(), matched, seq
}

func TestScannerNoStopSequences(t *testing.T) {
	emitted, matched, _ := drive(nil, []string{"hello ", "world"})
	if matched {
		t.Fatal("unexpected match with no stop sequences")
	}
	if emitted != "hello world" {
		t.Errorf("emitted = %q, want %q", emitted, "hello world")
	}
}

func TestScannerPassThroughDoesNotHoldBack(t *testing.T) {
	// With no stop sequences, each chunk must be emitted immediately and whole
	// (streaming must not buffer when there is nothing to match).
	s := NewStopSequenceScanner(nil)
	out, _ := s.Push("abc")
	if out != "abc" {
		t.Errorf("first push emitted %q, want %q", out, "abc")
	}
}

func TestScannerMatchWithinChunk(t *testing.T) {
	emitted, matched, seq := drive([]string{"STOP"}, []string{"keep thisSTOPdrop that"})
	if !matched || seq != "STOP" {
		t.Fatalf("matched=%v seq=%q, want true/STOP", matched, seq)
	}
	if emitted != "keep this" {
		t.Errorf("emitted = %q, want %q", emitted, "keep this")
	}
}

func TestScannerMatchSplitAcrossChunks(t *testing.T) {
	// "STOP" is split as "ST" + "OP"; the scanner must hold back and still match.
	emitted, matched, seq := drive([]string{"STOP"}, []string{"abcST", "OPxyz"})
	if !matched || seq != "STOP" {
		t.Fatalf("matched=%v seq=%q, want true/STOP", matched, seq)
	}
	if emitted != "abc" {
		t.Errorf("emitted = %q, want %q", emitted, "abc")
	}
}

func TestScannerEarliestSequenceWins(t *testing.T) {
	emitted, matched, seq := drive([]string{"WORLD", "lo"}, []string{"hello world"})
	if !matched {
		t.Fatal("expected a match")
	}
	if seq != "lo" || emitted != "hel" {
		t.Errorf("seq=%q emitted=%q, want lo/hel", seq, emitted)
	}
}

func TestScannerNoMatchFlushesTail(t *testing.T) {
	// The held-back tail must be emitted at clean end of stream.
	emitted, matched, _ := drive([]string{"STOP"}, []string{"the en", "d"})
	if matched {
		t.Fatal("unexpected match")
	}
	if emitted != "the end" {
		t.Errorf("emitted = %q, want %q", emitted, "the end")
	}
}

func TestScannerDoneAfterMatch(t *testing.T) {
	s := NewStopSequenceScanner([]string{"X"})
	out, hit := s.Push("aXb")
	if !hit || out != "a" {
		t.Fatalf("first push: out=%q hit=%v", out, hit)
	}
	out, hit = s.Push("more")
	if hit || out != "" {
		t.Errorf("after match: out=%q hit=%v, want empty/false", out, hit)
	}
	if s.Flush() != "" {
		t.Error("Flush after match should be empty")
	}
}

func TestScannerDoesNotSplitRune(t *testing.T) {
	// A 3-byte rune (€) arriving one byte at a time must never be emitted
	// partially: every emitted chunk must be valid UTF-8.
	euro := "€" // 3 bytes
	s := NewStopSequenceScanner([]string{"STOP"})
	var b strings.Builder
	for i := 0; i < len(euro); i++ {
		out, _ := s.Push(euro[i : i+1])
		if !isValidUTF8(out) {
			t.Fatalf("emitted invalid UTF-8 fragment %q", out)
		}
		b.WriteString(out)
	}
	b.WriteString(s.Flush())
	if b.String() != euro {
		t.Errorf("reassembled = %q, want %q", b.String(), euro)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' && s != "�" {
			return false
		}
	}
	return true
}

func TestScannerMatchesNonStreamingTruncation(t *testing.T) {
	// The scanner and ApplyStopSequences must agree on the surviving text.
	full := "alpha beta STOP gamma"
	seqs := []string{"STOP"}

	resp, seq := ApplyStopSequences(Response{Blocks: []ContentBlock{TextBlock(full)}}, seqs)
	emitted, matched, scanSeq := drive(seqs, []string{full})

	if !matched || scanSeq != seq {
		t.Fatalf("scanner seq=%q matched=%v, non-streaming seq=%q", scanSeq, matched, seq)
	}
	if emitted != resp.Text() {
		t.Errorf("scanner emitted %q, ApplyStopSequences kept %q", emitted, resp.Text())
	}
}
