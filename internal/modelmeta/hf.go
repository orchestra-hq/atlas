package modelmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/orchestra-hq/atlas/catalog"
)

// inspectHF reads a Hugging Face transformers repo's metadata files and derives
// its capabilities. config.json is required (its absence means the repo is not a
// recognized transformers — or GGUF — model); tokenizer_config.json and
// generation_config.json are optional and only enrich the record.
func inspectHF(ctx context.Context, repo string, opts Options) (Result, error) {
	cfgBytes, found, err := fetchFile(ctx, opts, repo, "config.json")
	if err != nil {
		return Result{}, err
	}
	if !found {
		// No config.json — the repo may instead hold GGUF weights. Detect and inspect
		// those before giving up.
		if res, ok, gerr := tryGGUFRepo(ctx, repo, opts); gerr != nil {
			return Result{}, gerr
		} else if ok {
			return res, nil
		}
		return Result{}, fmt.Errorf("modelmeta: %s has no config.json and no .gguf files — not a recognized transformers or GGUF repo", repo)
	}

	var cfg hfConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		return Result{}, fmt.Errorf("modelmeta: %s config.json: %w", repo, err)
	}

	caps := Capabilities{
		Repo:          repo,
		Revision:      opts.revision(),
		Format:        FormatSafetensors,
		Architecture:  cfg.architecture(),
		ModelType:     cfg.ModelType,
		ContextWindow: cfg.MaxPositionEmbeddings,
		RopeScaling:   cfg.ropeScalingNote(),
		Engines:       candidateEngines(FormatSafetensors),
	}

	// tokenizer_config.json → chat template presence (optional).
	if tokBytes, ok, err := fetchFile(ctx, opts, repo, "tokenizer_config.json"); err != nil {
		return Result{}, err
	} else if ok {
		var tok hfTokenizerConfig
		if err := json.Unmarshal(tokBytes, &tok); err != nil {
			return Result{}, fmt.Errorf("modelmeta: %s tokenizer_config.json: %w", repo, err)
		}
		caps.HasChatTemplate = tok.hasChatTemplate()
	}

	// generation_config.json → author sampling defaults (optional).
	if genBytes, ok, err := fetchFile(ctx, opts, repo, "generation_config.json"); err != nil {
		return Result{}, err
	} else if ok {
		var gen hfGenerationConfig
		if err := json.Unmarshal(genBytes, &gen); err != nil {
			return Result{}, fmt.Errorf("modelmeta: %s generation_config.json: %w", repo, err)
		}
		caps.Sampling = gen.sampling()
	}

	// Pre-download weight size for the fit check (M8 Phase 3): sum the repo's
	// safetensors shards. Best-effort — a listing failure (rate limit, older API)
	// just leaves WeightBytes at 0, which skips the fit check rather than failing
	// inspection.
	if files, err := listRepoFiles(ctx, opts, repo); err == nil {
		caps.WeightBytes = sumSafetensorBytes(files)
	}

	return Result{Capabilities: caps, Verdict: verdictFor(caps)}, nil
}

// sumSafetensorBytes totals the byte sizes of a repo's *.safetensors shards,
// ignoring tokenizer/config/readme files. 0 when none carry a known size.
func sumSafetensorBytes(files []repoFile) int64 {
	var sum int64
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.name), ".safetensors") {
			sum += f.size
		}
	}
	return sum
}

// fetchFile GETs one metadata file from repo at the configured revision. It
// returns (body, true, nil) on 200, (nil, false, nil) on 404 (the file is
// absent), and an actionable error on auth failure or any other status.
func fetchFile(ctx context.Context, opts Options, repo, file string) ([]byte, bool, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/%s", opts.endpoint(), repo, opts.revision(), file)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("modelmeta: request %s: %w", file, err)
	}
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.client().Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("modelmeta: fetch %s: %w", file, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, fmt.Errorf("modelmeta: read %s: %w", file, err)
		}
		return body, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, false, fmt.Errorf("modelmeta: %s is gated or private — set HF_TOKEN (or HUGGING_FACE_HUB_TOKEN) and accept the license at %s/%s", repo, opts.endpoint(), repo)
	default:
		return nil, false, fmt.Errorf("modelmeta: fetch %s: unexpected status %s", file, resp.Status)
	}
}

// hfConfig is the subset of config.json modelmeta reads.
type hfConfig struct {
	Architectures         []string       `json:"architectures"`
	ModelType             string         `json:"model_type"`
	MaxPositionEmbeddings int            `json:"max_position_embeddings"`
	RopeScaling           map[string]any `json:"rope_scaling"`
}

func (c hfConfig) architecture() string {
	if len(c.Architectures) > 0 {
		return c.Architectures[0]
	}
	return ""
}

// ropeScalingNote summarizes rope_scaling for display when context is extended
// beyond the base window (e.g. "yarn x4"); empty when the model uses none.
func (c hfConfig) ropeScalingNote() string {
	if len(c.RopeScaling) == 0 {
		return ""
	}
	typ, _ := c.RopeScaling["rope_type"].(string)
	if typ == "" {
		typ, _ = c.RopeScaling["type"].(string)
	}
	if typ == "" {
		typ = "scaled"
	}
	if factor, ok := c.RopeScaling["factor"].(float64); ok {
		return fmt.Sprintf("%s x%g", typ, factor)
	}
	return typ
}

// hfTokenizerConfig is the subset of tokenizer_config.json modelmeta reads. The
// chat_template field is a string in most repos but a list of named templates in
// a few, so it is decoded loosely.
type hfTokenizerConfig struct {
	ChatTemplate any `json:"chat_template"`
}

func (t hfTokenizerConfig) hasChatTemplate() bool {
	switch v := t.ChatTemplate.(type) {
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	default:
		return false
	}
}

// hfGenerationConfig is the subset of generation_config.json modelmeta reads.
type hfGenerationConfig struct {
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
}

func (g hfGenerationConfig) sampling() catalog.Sampling {
	return catalog.Sampling{Temperature: g.Temperature, TopP: g.TopP}
}
