package modelmeta

import (
	"encoding/binary"
	"errors"
	"strings"
)

// GGUF is a little-endian binary format: a magic+version+counts header followed
// by a metadata key/value store, then tensor info and the weights. Spec:
// https://github.com/ggml-org/ggml/blob/master/docs/gguf.md. We parse only the
// metadata KV store (it sits at the front) to derive architecture, context
// window, and chat-template presence — never reading the tensors or weights.

// errShortHeader signals the buffer ended mid-parse, so the caller should fetch
// a larger window and retry (the metadata exceeded the read).
var errShortHeader = errors.New("modelmeta: gguf header truncated")

var errNotGGUF = errors.New("modelmeta: not a valid GGUF file (bad magic)")

// maxGGUFField bounds a single declared string length or array element count. The
// whole header we ever fetch is capped at ggufMaxHeaderBytes, so any field larger
// than that is malformed — rejecting it avoids growing the fetch window chasing a
// bogus length.
const maxGGUFField = ggufMaxHeaderBytes

// GGUF metadata value types.
const (
	ggufUint8 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

type ggufMeta struct {
	architecture    string
	contextWindow   int
	hasChatTemplate bool
}

// parseGGUFHeader reads the metadata KV store and extracts the fields inspection
// needs. It returns errShortHeader if the buffer is too small (grow and retry)
// and errNotGGUF if the magic is wrong.
func parseGGUFHeader(b []byte) (ggufMeta, error) {
	c := &ggufCursor{b: b}
	magic, err := c.take(4)
	if err != nil {
		return ggufMeta{}, err
	}
	if string(magic) != "GGUF" {
		return ggufMeta{}, errNotGGUF
	}
	if _, err := c.u32(); err != nil { // version
		return ggufMeta{}, err
	}
	if _, err := c.u64(); err != nil { // tensor count (unused: we stop before tensor info)
		return ggufMeta{}, err
	}
	kvCount, err := c.u64()
	if err != nil {
		return ggufMeta{}, err
	}

	var meta ggufMeta
	for i := uint64(0); i < kvCount; i++ {
		key, err := c.str()
		if err != nil {
			return ggufMeta{}, err
		}
		val, err := c.value()
		if err != nil {
			return ggufMeta{}, err
		}
		switch {
		case key == "general.architecture":
			if s, ok := val.(string); ok {
				meta.architecture = s
			}
		case key == "tokenizer.chat_template":
			if s, ok := val.(string); ok && s != "" {
				meta.hasChatTemplate = true
			}
		case strings.HasSuffix(key, ".context_length"):
			if n, ok := val.(int64); ok {
				meta.contextWindow = int(n)
			}
		}
	}
	return meta, nil
}

// ggufCursor reads little-endian values off a byte slice, returning errShortHeader
// the moment a read would run past the end.
type ggufCursor struct {
	b []byte
	i int
}

func (c *ggufCursor) take(n int) ([]byte, error) {
	// Compare without c.i+n so an attacker-controlled n near MaxInt can't overflow
	// the sum to a negative value that slips past the guard and panics the slice.
	// The invariant c.i <= len(c.b) keeps len(c.b)-c.i non-negative.
	if n < 0 || n > len(c.b)-c.i {
		return nil, errShortHeader
	}
	out := c.b[c.i : c.i+n]
	c.i += n
	return out, nil
}

func (c *ggufCursor) u32() (uint32, error) {
	b, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *ggufCursor) u64() (uint64, error) {
	b, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (c *ggufCursor) str() (string, error) {
	n, err := c.u64()
	if err != nil {
		return "", err
	}
	// A length beyond any plausible header is malformed, not "fetch more" — reject
	// it outright so a bogus length doesn't drive the fetch window up to the cap.
	if n > maxGGUFField {
		return "", errNotGGUF
	}
	b, err := c.take(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// value reads one metadata value (a leading type tag then the payload),
// advancing the cursor past it. It returns the Go value for the types inspection
// cares about (string, int64) and nil for the rest (still fully consumed).
func (c *ggufCursor) value() (any, error) {
	t, err := c.u32()
	if err != nil {
		return nil, err
	}
	return c.valueOf(t)
}

func (c *ggufCursor) valueOf(t uint32) (any, error) {
	switch t {
	case ggufUint8, ggufInt8, ggufBool:
		_, err := c.take(1)
		return nil, err
	case ggufUint16, ggufInt16:
		_, err := c.take(2)
		return nil, err
	case ggufUint32:
		v, err := c.u32()
		return int64(v), err
	case ggufInt32:
		v, err := c.u32()
		return int64(int32(v)), err
	case ggufFloat32:
		_, err := c.take(4)
		return nil, err
	case ggufUint64, ggufInt64:
		v, err := c.u64()
		return int64(v), err //nolint:gosec // context_length fits comfortably in int64
	case ggufFloat64:
		_, err := c.take(8)
		return nil, err
	case ggufString:
		return c.str()
	case ggufArray:
		elemType, err := c.u32()
		if err != nil {
			return nil, err
		}
		n, err := c.u64()
		if err != nil {
			return nil, err
		}
		// An element count exceeding the header cap is malformed (each element is at
		// least one byte), so reject rather than loop/grow toward the bogus count.
		if n > maxGGUFField {
			return nil, errNotGGUF
		}
		for j := uint64(0); j < n; j++ {
			if _, err := c.valueOf(elemType); err != nil {
				return nil, err
			}
		}
		return nil, nil // arrays are skipped, not captured
	default:
		return nil, errNotGGUF // unknown value type → treat as malformed
	}
}
