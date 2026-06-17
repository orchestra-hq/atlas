// Package store is the content-addressable model cache: weight blobs keyed by
// their sha256 digest, plus a manifest per logical model name that points at a
// blob and records where it came from. This keeps all model state under one
// directory (build-time decision 3) and lets a pull be idempotent and verified,
// the same shape Ollama/OCI use (architecture.md, "Model storage").
//
// M0 stores single-file GGUF weights for llama.cpp. Multi-file Hugging Face
// repos (vLLM) are fetched by the engine itself on first boot and do not pass
// through this store; see internal/cli for how catalog entries route by engine.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// nameRE constrains logical model names to characters safe as a single path
// component, so a manifest file name can never escape the manifests dir.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Manifest records one stored model: the blob it resolves to and the source it
// was pulled from. It is the unit written under manifests/<name>.json.
type Manifest struct {
	Name string `json:"name"`
	// Engine is the engine the weights are for (e.g. "llamacpp"); informational
	// in M0 since only GGUF blobs are stored.
	Engine string `json:"engine"`
	// Digest is the blob's content digest, "sha256:<hex>".
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	// Source is the origin URL the blob was pulled from.
	Source string `json:"source"`
	// ContextWindow is the model's context length in tokens, carried from the
	// catalog as a hint; the gateway still confirms it against the live engine.
	ContextWindow int       `json:"context_window,omitempty"`
	PulledAt      time.Time `json:"pulled_at"`
}

// Store is a content-addressable model cache rooted at Dir. The zero value is
// not usable; construct with New.
type Store struct {
	// Dir is the store root, e.g. <state>/store. Blobs live under blobs/ and
	// manifests under manifests/.
	Dir string
	// Client fetches blobs during Pull; nil uses http.DefaultClient.
	Client *http.Client
	// Progress, when set, is called periodically during a Pull download with the
	// bytes fetched so far and the total (0 if unknown). For CLI feedback only.
	Progress func(done, total int64)
}

// New returns a store rooted at dir.
func New(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) blobsDir() string     { return filepath.Join(s.Dir, "blobs") }
func (s *Store) manifestsDir() string { return filepath.Join(s.Dir, "manifests") }

// blobPath maps a "sha256:<hex>" digest to its on-disk path. The colon becomes
// a dash so the digest is a valid filename on every platform.
func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.blobsDir(), strings.ReplaceAll(digest, ":", "-"))
}

func (s *Store) manifestPath(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("store: invalid model name %q", name)
	}
	return filepath.Join(s.manifestsDir(), name+".json"), nil
}

// Get returns the manifest for a stored model, or an error if it is absent.
func (s *Store) Get(name string) (Manifest, error) {
	path, err := s.manifestPath(name)
	if err != nil {
		return Manifest{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, fmt.Errorf("store: model %q not pulled", name)
		}
		return Manifest{}, fmt.Errorf("store: read manifest %q: %w", name, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("store: parse manifest %q: %w", name, err)
	}
	return m, nil
}

// Has reports whether a model is fully present: its manifest exists and the blob
// it references is on disk. A manifest whose blob is missing (a half-cleaned
// store) reads as absent so the caller re-pulls.
func (s *Store) Has(name string) bool {
	m, err := s.Get(name)
	if err != nil {
		return false
	}
	if _, err := os.Stat(s.blobPath(m.Digest)); err != nil {
		return false
	}
	return true
}

// Path returns the on-disk blob path for a stored model — what the worker hands
// the engine (llama.cpp's -m). It errors if the model is not fully present.
func (s *Store) Path(name string) (string, error) {
	m, err := s.Get(name)
	if err != nil {
		return "", err
	}
	p := s.blobPath(m.Digest)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("store: blob for %q missing: %w", name, err)
	}
	return p, nil
}

// PullSpec describes a single-file weight blob to fetch into the store.
type PullSpec struct {
	Name          string
	Engine        string
	URL           string
	SHA256        string // hex, required: the pinned digest to verify against
	Size          int64  // expected bytes, for progress display (optional)
	ContextWindow int
}

// Pull downloads spec.URL into the store, verifies its sha256 against
// spec.SHA256, and writes the manifest. It is idempotent: if the model is
// already present with the same digest the download is skipped. The bytes are
// streamed to a temp file while hashing, so a mismatched or interrupted
// download never becomes a trusted blob.
func (s *Store) Pull(ctx context.Context, spec PullSpec) (Manifest, error) {
	if spec.SHA256 == "" {
		return Manifest{}, fmt.Errorf("store: pull %q: no pinned sha256", spec.Name)
	}
	digest := "sha256:" + spec.SHA256
	if existing, err := s.Get(spec.Name); err == nil && existing.Digest == digest {
		if _, err := os.Stat(s.blobPath(digest)); err == nil {
			return existing, nil // already pulled and verified
		}
	}

	if err := os.MkdirAll(s.blobsDir(), 0o755); err != nil {
		return Manifest{}, fmt.Errorf("store: create blobs dir: %w", err)
	}

	got, size, err := s.download(ctx, spec, s.blobPath(digest))
	if err != nil {
		return Manifest{}, err
	}
	if got != spec.SHA256 {
		return Manifest{}, fmt.Errorf("store: checksum mismatch for %q: got %s, want %s", spec.Name, got, spec.SHA256)
	}

	m := Manifest{
		Name:          spec.Name,
		Engine:        spec.Engine,
		Digest:        digest,
		Size:          size,
		Source:        spec.URL,
		ContextWindow: spec.ContextWindow,
		PulledAt:      time.Now().UTC(),
	}
	if err := s.writeManifest(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// download streams spec.URL to a temp file while hashing, then atomically
// renames it to dest. It returns the computed hex digest and the byte count.
func (s *Store) download(ctx context.Context, spec PullSpec, dest string) (digest string, size int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("store: build request: %w", err)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("store: download %q: %w", spec.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("store: download %q: HTTP %d", spec.Name, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(s.blobsDir(), "incoming-*")
	if err != nil {
		return "", 0, fmt.Errorf("store: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	total := spec.Size
	if total == 0 && resp.ContentLength > 0 {
		total = resp.ContentLength
	}
	hasher := sha256.New()
	src := io.TeeReader(resp.Body, hasher)
	if s.Progress != nil {
		src = io.TeeReader(src, &progressWriter{total: total, report: s.Progress})
	}
	n, err := io.Copy(tmp, src)
	if err != nil {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("store: write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("store: close blob: %w", err)
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != spec.SHA256 {
		return got, n, nil // caller reports the mismatch; temp file is cleaned up
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", 0, fmt.Errorf("store: install blob: %w", err)
	}
	return got, n, nil
}

func (s *Store) writeManifest(m Manifest) error {
	if err := os.MkdirAll(s.manifestsDir(), 0o755); err != nil {
		return fmt.Errorf("store: create manifests dir: %w", err)
	}
	path, err := s.manifestPath(m.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode manifest: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("store: write manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: install manifest: %w", err)
	}
	return nil
}

// progressWriter reports cumulative download progress through Store.Progress.
type progressWriter struct {
	done   int64
	total  int64
	report func(done, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	w.report(w.done, w.total)
	return len(p), nil
}
