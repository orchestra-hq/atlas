// Package catalog is Atlas's curated starter model catalog: agent-tested model
// definitions seeded from docs/research/model-catalog-m0.md and pinned at build
// time (build-time decision 5). The definitions are embedded so the static
// binary needs no external files; the gateway and `atlas pull`/`atlas up`
// resolve a model name through here to its source, engine, and serving config.
package catalog

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	_ "embed"

	"gopkg.in/yaml.v3"
)

//go:embed starter.yaml
var starterYAML []byte

// nameRE constrains catalog names to a single safe path component so they can
// double as store manifest names and served model ids.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var (
	validEngines = map[string]bool{"llamacpp": true, "vllm": true, "mlx": true, "sglang": true}
	validTiers   = map[string]bool{"haiku": true, "sonnet": true, "opus": true}
	validSources = map[string]bool{"gguf": true, "hf": true}
	validClasses = map[string]bool{ClassChat: true, ClassEmbedding: true, ClassReranker: true}
	sha256RE     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Model classes (M3 phase 2a, ADR-0012). A model's class selects the endpoint and
// engine capability a request routes to: ClassChat is the default and covers every
// model that existed before classes, so an entry that omits `class` is chat and
// nothing about the existing catalog changes. ClassEmbedding serves /v1/embeddings;
// ClassReranker (phase 2b) serves /v1/rerank.
const (
	ClassChat      = "chat"
	ClassEmbedding = "embedding"
	ClassReranker  = "reranker"
)

// Source is where a model's weights come from. A "gguf" source is a single
// pinned file the store fetches and verifies; an "hf" source is a Hugging Face
// repo the engine (vLLM) resolves itself at boot.
type Source struct {
	Type   string `yaml:"type"`
	URL    string `yaml:"url,omitempty"`
	Repo   string `yaml:"repo,omitempty"`
	SHA256 string `yaml:"sha256,omitempty"`
	Size   int64  `yaml:"size,omitempty"`
}

// Sampling holds per-model sampling defaults from the model's authors (wrong
// defaults visibly degrade tool calling — see model-catalog-m0.md, finding 3).
// Recorded with the entry; applied when a request omits the field.
type Sampling struct {
	Temperature *float64 `yaml:"temperature,omitempty"`
	TopP        *float64 `yaml:"top_p,omitempty"`
}

// Entry is one curated model definition.
type Entry struct {
	Name string `yaml:"name"`
	// Class selects the model's role: "chat" (default), "embedding", or "reranker"
	// (M3 phase 2a, ADR-0012). Empty means chat, so existing entries are unchanged.
	Class         string   `yaml:"class,omitempty"`
	Engine        string   `yaml:"engine"`
	Tier          string   `yaml:"tier,omitempty"`
	Reasoning     bool     `yaml:"reasoning"`
	ContextWindow int      `yaml:"context_window"`
	Source        Source   `yaml:"source"`
	EngineArgs    []string `yaml:"engine_args,omitempty"`
	Sampling      Sampling `yaml:"sampling,omitempty"`
}

// ClassOrChat returns the entry's model class, defaulting an unset field to "chat"
// so callers never have to special-case the empty string.
func (e Entry) ClassOrChat() string {
	if e.Class == "" {
		return ClassChat
	}
	return e.Class
}

type catalogFile struct {
	Models []Entry `yaml:"models"`
}

// Catalog is a validated set of model entries, indexed by name.
type Catalog struct {
	entries []Entry
	byName  map[string]Entry
}

// Load parses and validates the embedded starter catalog. A malformed catalog
// is a build-time error in the binary, so callers may treat an error here as
// fatal; the catalog_test asserts the shipped data is valid.
func Load() (*Catalog, error) {
	return parse(starterYAML)
}

// LoadFile parses and validates a catalog from a YAML file on disk, for tests
// and (later) user-supplied catalogs.
func LoadFile(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s: %w", path, err)
	}
	return parse(data)
}

func parse(data []byte) (*Catalog, error) {
	var cf catalogFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}
	c := &Catalog{byName: make(map[string]Entry, len(cf.Models))}
	for i := range cf.Models {
		e := cf.Models[i]
		if err := validate(e); err != nil {
			return nil, err
		}
		if _, dup := c.byName[e.Name]; dup {
			return nil, fmt.Errorf("catalog: duplicate model name %q", e.Name)
		}
		c.byName[e.Name] = e
		c.entries = append(c.entries, e)
	}
	return c, nil
}

func validate(e Entry) error {
	if !nameRE.MatchString(e.Name) {
		return fmt.Errorf("catalog: invalid model name %q", e.Name)
	}
	if !validEngines[e.Engine] {
		return fmt.Errorf("catalog: model %q: invalid engine %q", e.Name, e.Engine)
	}
	if !validClasses[e.ClassOrChat()] {
		return fmt.Errorf("catalog: model %q: invalid class %q (want chat|embedding|reranker)", e.Name, e.Class)
	}
	// Tier is the chat alias key (claude-sonnet → a sonnet-tier model), so it is
	// required for chat models and inapplicable to embedding/reranker models, which
	// are addressed by name only. A non-chat entry may omit it, but a present tier
	// must still be valid.
	if e.ClassOrChat() == ClassChat {
		if !validTiers[e.Tier] {
			return fmt.Errorf("catalog: model %q: invalid tier %q (want haiku|sonnet|opus)", e.Name, e.Tier)
		}
	} else if e.Tier != "" && !validTiers[e.Tier] {
		return fmt.Errorf("catalog: model %q: invalid tier %q (want haiku|sonnet|opus)", e.Name, e.Tier)
	}
	if e.ContextWindow <= 0 {
		return fmt.Errorf("catalog: model %q: context_window must be positive", e.Name)
	}
	if !validSources[e.Source.Type] {
		return fmt.Errorf("catalog: model %q: invalid source type %q", e.Name, e.Source.Type)
	}
	switch e.Source.Type {
	case "gguf":
		if e.Engine != "llamacpp" {
			return fmt.Errorf("catalog: model %q: gguf source requires engine llamacpp", e.Name)
		}
		if e.Source.URL == "" {
			return fmt.Errorf("catalog: model %q: gguf source needs a url", e.Name)
		}
		if !sha256RE.MatchString(e.Source.SHA256) {
			return fmt.Errorf("catalog: model %q: gguf source needs a 64-hex sha256 (pin from day one)", e.Name)
		}
	case "hf":
		if e.Source.Repo == "" {
			return fmt.Errorf("catalog: model %q: hf source needs a repo", e.Name)
		}
	}
	return nil
}

// Lookup returns the entry for name, and whether it exists.
func (c *Catalog) Lookup(name string) (Entry, bool) {
	e, ok := c.byName[name]
	return e, ok
}

// All returns the entries in catalog order.
func (c *Catalog) All() []Entry { return c.entries }

// Summary renders a one-line description of an entry for CLI listing.
func (e Entry) Summary() string {
	src := e.Source.Repo
	if e.Source.Type == "gguf" {
		src = "gguf"
	}
	think := "no-think"
	if e.Reasoning {
		think = "thinking"
	}
	return fmt.Sprintf("%-22s %-9s %-7s %-9s %s", e.Name, e.Engine, e.Tier, think, src)
}

// Header is the column header matching Entry.Summary, for listings.
func Header() string {
	return fmt.Sprintf("%-22s %-9s %-7s %-9s %s",
		strings.ToUpper("name"), strings.ToUpper("engine"),
		strings.ToUpper("tier"), strings.ToUpper("think"), strings.ToUpper("source"))
}
