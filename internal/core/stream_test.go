package core

import (
	"strings"
	"testing"
)

// recordingSink captures the StreamSink calls it receives, so a StreamEvent's
// ApplyTo mapping can be asserted against the method it should invoke.
type recordingSink struct {
	calls []string
}

func (r *recordingSink) Thinking(d string) error {
	r.calls = append(r.calls, "thinking:"+d)
	return nil
}
func (r *recordingSink) Text(d string) error { r.calls = append(r.calls, "text:"+d); return nil }
func (r *recordingSink) ToolCallStart(_ int, id, name string) error {
	r.calls = append(r.calls, "start:"+id+":"+name)
	return nil
}

func (r *recordingSink) ToolCallDelta(_ int, f string) error {
	r.calls = append(r.calls, "delta:"+f)
	return nil
}
func (r *recordingSink) Done(StopReason, Usage) error { r.calls = append(r.calls, "done"); return nil }

// TestStreamEvent_emit_apply_roundtrip drives an EventSink (the worker side)
// and replays the captured events through ApplyTo (the gateway side), asserting
// the deltas land on the matching sink methods in order — the core of the
// worker-channel stream path.
func TestStreamEvent_emit_apply_roundtrip(t *testing.T) {
	var events []StreamEvent
	emit := EventSink{
		Emit:   func(e StreamEvent) error { events = append(events, e); return nil },
		OnDone: func(StopReason, Usage) error { return nil },
	}
	// Drive the emitting sink as an engine adapter would.
	_ = emit.Thinking("reason")
	_ = emit.Text("hello ")
	_ = emit.ToolCallStart(0, "tu_1", "get_weather")
	_ = emit.ToolCallDelta(0, `{"city":`)
	_ = emit.ToolCallDelta(0, `"NYC"}`)

	rec := &recordingSink{}
	for _, e := range events {
		if err := e.ApplyTo(rec); err != nil {
			t.Fatalf("ApplyTo(%+v): %v", e, err)
		}
	}

	want := []string{"thinking:reason", "text:hello ", "start:tu_1:get_weather", "delta:{\"city\":", "delta:\"NYC\"}"}
	if strings.Join(rec.calls, "|") != strings.Join(want, "|") {
		t.Errorf("sink calls = %v, want %v", rec.calls, want)
	}
}

// TestStreamEvent_applyto_propagates_sink_error confirms ApplyTo returns the
// sink's error unchanged, so ErrStopStreaming reaches the remote-stream caller.
func TestStreamEvent_applyto_propagates_sink_error(t *testing.T) {
	sink := stopOnTextSink{}
	err := StreamEvent{Kind: EventText, Text: "x"}.ApplyTo(sink)
	if err != ErrStopStreaming {
		t.Errorf("ApplyTo error = %v, want ErrStopStreaming", err)
	}
}

type stopOnTextSink struct{}

func (stopOnTextSink) Thinking(string) error                   { return nil }
func (stopOnTextSink) Text(string) error                       { return ErrStopStreaming }
func (stopOnTextSink) ToolCallStart(int, string, string) error { return nil }
func (stopOnTextSink) ToolCallDelta(int, string) error         { return nil }
func (stopOnTextSink) Done(StopReason, Usage) error            { return nil }

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
