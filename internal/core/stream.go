package core

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// StreamSink receives a streaming response incrementally. An engine adapter
// emits zero or more Text deltas, then exactly one Done, in order. The gateway
// implements a sink that translates these into Anthropic SSE events.
//
// A sink method may return ErrStopStreaming to ask the engine to stop
// generating and end the stream cleanly (the gateway uses this when a stop
// sequence matches mid-stream); the engine treats that as success, not failure.
type StreamSink interface {
	// Text reports the next chunk of generated text. delta is never empty.
	Text(delta string) error
	// Done reports normal end of generation with the engine's stop reason and
	// final cumulative usage.
	Done(reason StopReason, usage Usage) error
}

// ErrStopStreaming is returned by a StreamSink method to tell the engine to
// stop generating and end the stream without error. The gateway returns it
// from Text once a stop sequence has been matched.
var ErrStopStreaming = errors.New("core: stop streaming")

// StopSequenceScanner enforces Anthropic stop-sequence semantics over a stream
// of text chunks, mirroring ApplyStopSequences for the non-streaming path so a
// client sees identical truncation either way. The gateway owns this rather
// than each engine, so behavior is engine-independent.
//
// It buffers a short tail (just under the longest stop sequence) so a sequence
// split across chunk boundaries is still caught, and never cuts a chunk in the
// middle of a UTF-8 rune (a half rune would be mangled by JSON encoding). Once
// a stop sequence matches, output is truncated at the match and the scanner is
// done: further Push calls emit nothing.
type StopSequenceScanner struct {
	seqs    []string
	holdNum int // bytes to hold back: longest sequence minus one
	pending string
	done    bool
	matched string
}

// NewStopSequenceScanner builds a scanner for the given stop sequences. Empty
// sequences are ignored; a nil or empty list yields a pass-through scanner.
func NewStopSequenceScanner(seqs []string) *StopSequenceScanner {
	maxLen := 0
	kept := make([]string, 0, len(seqs))
	for _, s := range seqs {
		if s == "" {
			continue
		}
		kept = append(kept, s)
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}
	hold := 0
	if maxLen > 1 {
		hold = maxLen - 1
	}
	return &StopSequenceScanner{seqs: kept, holdNum: hold}
}

// Push feeds the next chunk and returns the text that is now safe to emit
// (possibly empty). matched is true exactly once, on the call where a stop
// sequence is first found; the returned text is truncated at the match and the
// scanner is done thereafter. Call Matched for the sequence that fired.
func (s *StopSequenceScanner) Push(chunk string) (emit string, matched bool) {
	if s.done || len(s.seqs) == 0 {
		if s.done {
			return "", false
		}
		// No stop sequences: pass through immediately, never holding back.
		return chunk, false
	}

	s.pending += chunk

	if idx, seq := s.earliestMatch(); idx >= 0 {
		out := s.pending[:idx]
		s.pending = ""
		s.done = true
		s.matched = seq
		return out, true
	}

	// No full match yet. Emit everything except the trailing holdNum bytes
	// (which might start a sequence completed by a later chunk), without
	// cutting a rune.
	cut := len(s.pending) - s.holdNum
	if cut <= 0 {
		return "", false
	}
	cut = runeSafeCut(s.pending, cut)
	if cut <= 0 {
		return "", false
	}
	out := s.pending[:cut]
	s.pending = s.pending[cut:]
	return out, false
}

// Flush returns any held-back tail at end of stream. It is valid to call only
// when the engine ended without a stop-sequence match; after a match it returns
// the empty string (the tail was already discarded as truncated output).
func (s *StopSequenceScanner) Flush() string {
	if s.done {
		return ""
	}
	out := s.pending
	s.pending = ""
	return out
}

// Matched returns the stop sequence that fired, or "" if none did.
func (s *StopSequenceScanner) Matched() string { return s.matched }

// earliestMatch finds the earliest-occurring stop sequence in pending.
func (s *StopSequenceScanner) earliestMatch() (int, string) {
	best := -1
	var bestSeq string
	for _, seq := range s.seqs {
		if idx := strings.Index(s.pending, seq); idx >= 0 && (best < 0 || idx < best) {
			best, bestSeq = idx, seq
		}
	}
	return best, bestSeq
}

// runeSafeCut returns the largest index <= n that does not fall in the middle
// of a UTF-8 rune in s, so s[:cut] is always valid UTF-8.
func runeSafeCut(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}
