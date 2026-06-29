package modelmeta

import (
	"fmt"
	"regexp"
	"strings"
)

// quantRE matches a GGUF quantization designator in a filename, e.g. Q4_K_M,
// Q8_0, IQ3_XXS, F16, BF16 — used to derive the quant tag from a chosen file and
// to summarize a repo's available quants.
var quantRE = regexp.MustCompile(`(?i)\b(IQ\d+[A-Z0-9_]*|Q\d+[A-Z0-9_]*|BF16|F16|F32)\b`)

// QuantToken extracts the quant designator from a GGUF filename (e.g.
// "Qwen3-8B-Q4_K_M.gguf" -> "Q4_K_M"), uppercased; "" when none is recognizable.
func QuantToken(file string) string {
	m := quantRE.FindAllString(file, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.ToUpper(m[len(m)-1]) // the quant tag sits at the tail of the name
}

// quantTokens lists the distinct quant designators across files, first-seen
// order, for an actionable error message.
func quantTokens(files []string) []string {
	seen := map[string]bool{}
	var toks []string
	for _, f := range files {
		if t := QuantToken(f); t != "" && !seen[t] {
			seen[t] = true
			toks = append(toks, t)
		}
	}
	return toks
}

// DefaultQuantToken is the canonical quant tag for the file inspection chose
// (Selected, Q4_K_M-preferring per ADR-0015); "" when there is no multi-quant
// selection or the filename carries no recognizable quant.
func (c Capabilities) DefaultQuantToken() string {
	return QuantToken(c.Selected)
}

// ResolveQuantToken maps a requested quant to a single canonical token from the
// repo's files, normalizing case and rejecting an absent or ambiguous request so
// a caller hands the engine a tag that names exactly one quantization. A request
// "matches" a file when (case-insensitively) the file name contains it; the
// distinct quant tokens of the matching files must come to exactly one.
func (c Capabilities) ResolveQuantToken(want string) (string, error) {
	w := strings.ToUpper(strings.TrimSpace(want))
	seen := map[string]bool{}
	var matched []string
	for _, f := range c.Files {
		tok := QuantToken(f)
		if tok == "" || !strings.Contains(strings.ToUpper(f), w) || seen[tok] {
			continue
		}
		seen[tok] = true
		matched = append(matched, tok)
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", fmt.Errorf("%q matches no quantization; available: %s", want, strings.Join(quantTokens(c.Files), ", "))
	default:
		return "", fmt.Errorf("%q is ambiguous; matches %s — be more specific", want, strings.Join(matched, ", "))
	}
}
