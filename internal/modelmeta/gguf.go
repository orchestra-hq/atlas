package modelmeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

// inspectGGUF reads a GGUF file's header to derive capabilities without
// downloading the weights. The target is a local .gguf path or a direct .gguf
// URL; an HF *repo* of GGUF files is detected on the transformers path (hf.go,
// tryGGUFRepo) and routed here per file.
func inspectGGUF(ctx context.Context, target string, opts Options) (Result, error) {
	switch {
	case isHTTPURL(target):
		data, total, err := fetchGGUFHeader(ctx, opts, target)
		if err != nil {
			return Result{}, err
		}
		res, err := ggufResult(target, opts.revision(), "", nil, data)
		if err != nil {
			return Result{}, err
		}
		res.Capabilities.WeightBytes = total // from the ranged fetch's Content-Range
		return res, nil
	case fileExists(target):
		data, err := readLocalGGUFHeader(target)
		if err != nil {
			return Result{}, err
		}
		res, err := ggufResult(target, opts.revision(), "", nil, data)
		if err != nil {
			return Result{}, err
		}
		if info, serr := os.Stat(target); serr == nil {
			res.Capabilities.WeightBytes = info.Size()
		}
		return res, nil
	default:
		return Result{}, fmt.Errorf("modelmeta: %q is not a local .gguf file or a URL — pass a path, a direct .gguf URL, or an HF repo id (for a multi-quant repo)", target)
	}
}

// tryGGUFRepo handles an HF repo whose weights are GGUF rather than safetensors:
// it lists the repo's .gguf files, picks one by the default heuristic
// (Q4_K_M-preferring), and inspects that file's header over a ranged fetch. It
// returns ok=false when the repo has no .gguf files (so the caller can fall back
// to its "not a recognized repo" error).
func tryGGUFRepo(ctx context.Context, repo string, opts Options) (Result, bool, error) {
	files, err := listRepoFiles(ctx, opts, repo)
	if err != nil {
		return Result{}, false, err
	}
	var ggufs []string
	sizes := map[string]int64{}
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.name), ".gguf") {
			ggufs = append(ggufs, f.name)
			sizes[f.name] = f.size
		}
	}
	if len(ggufs) == 0 {
		return Result{}, false, nil
	}
	sort.Strings(ggufs)
	chosen := pickQuant(ggufs)

	url := fmt.Sprintf("%s/%s/resolve/%s/%s", opts.endpoint(), repo, opts.revision(), chosen)
	data, _, err := fetchGGUFHeader(ctx, opts, url)
	if err != nil {
		return Result{}, false, err
	}
	res, err := ggufResult(repo, opts.revision(), chosen, ggufs, data)
	if err != nil {
		return Result{}, false, err
	}
	res.Capabilities.WeightBytes = sizes[chosen] // the served quant's size (M8 Phase 3 fit)
	return res, true, nil
}

// pickQuant chooses which quantization to inspect when a repo holds several:
// prefer a Q4_K_M build (the documented default, ADR-0015), else the first by
// name. The list is assumed sorted.
func pickQuant(ggufs []string) string {
	for _, f := range ggufs {
		if strings.Contains(strings.ToUpper(f), "Q4_K_M") {
			return f
		}
	}
	return ggufs[0]
}

// ggufResult parses a header blob into a Result, attaching the repo/file listing
// for display when inspecting a multi-quant repo.
func ggufResult(repo, revision, selected string, files []string, header []byte) (Result, error) {
	meta, err := parseGGUFHeader(header)
	if err != nil {
		return Result{}, err
	}
	caps := Capabilities{
		Repo:            repo,
		Revision:        revision,
		Format:          FormatGGUF,
		Architecture:    meta.architecture,
		ModelType:       meta.architecture, // GGUF carries one architecture key; model_type mirrors it
		ContextWindow:   meta.contextWindow,
		HasChatTemplate: meta.hasChatTemplate,
		Engines:         candidateEngines(FormatGGUF),
		Files:           files,
		Selected:        selected,
	}
	return Result{Capabilities: caps, Verdict: verdictFor(caps)}, nil
}

// repoFile is one file in an HF repo listing: its name and, when the API reports
// it (?blobs=true), its byte size (0 when unknown — older repos or non-LFS files
// can omit it).
type repoFile struct {
	name string
	size int64
}

// listRepoFiles returns the files in an HF repo via its model API
// (…/api/models/<repo>?blobs=true → siblings[].rfilename + .size). Used to detect
// a GGUF repo, enumerate its quantizations, and sum weight sizes for the
// pre-download fit check (M8 Phase 3). The blobs param adds sizes; a repo or file
// that omits a size simply reports 0 (fit then skipped).
func listRepoFiles(ctx context.Context, opts Options, repo string) ([]repoFile, error) {
	url := fmt.Sprintf("%s/api/models/%s?blobs=true", opts.endpoint(), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("modelmeta: request repo listing: %w", err)
	}
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("modelmeta: list %s: %w", repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Siblings []struct {
				RFilename string `json:"rfilename"`
				Size      int64  `json:"size"`
				LFS       struct {
					Size int64 `json:"size"`
				} `json:"lfs"`
			} `json:"siblings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("modelmeta: parse %s listing: %w", repo, err)
		}
		files := make([]repoFile, 0, len(body.Siblings))
		for _, s := range body.Siblings {
			size := s.Size
			if size == 0 {
				size = s.LFS.Size // weight shards are LFS-tracked; size lives under lfs
			}
			files = append(files, repoFile{name: s.RFilename, size: size})
		}
		return files, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("modelmeta: %s is gated or private — set HF_TOKEN (or HUGGING_FACE_HUB_TOKEN) and accept the license at %s/%s", repo, opts.endpoint(), repo)
	case http.StatusNotFound:
		return nil, fmt.Errorf("modelmeta: %s not found", repo)
	default:
		return nil, fmt.Errorf("modelmeta: list %s: unexpected status %s", repo, resp.Status)
	}
}

// --- header fetch (bounded; never downloads weights) ---

const (
	ggufInitialHeaderBytes = 1 << 20  // 1 MiB: covers typical metadata + embedded template
	ggufMaxHeaderBytes     = 64 << 20 // grow cap for unusually large embedded templates
)

// fetchGGUFHeader reads only enough of a GGUF file (from the front, via HTTP
// Range) to parse its metadata, growing the window if the header runs past the
// first read. It never fetches the whole file: even if a server ignores Range,
// the body read is capped at the current window. It also returns the file's total
// size (from the Content-Range total, 0 when the server does not report it) for
// the pre-download fit check.
func fetchGGUFHeader(ctx context.Context, opts Options, url string) ([]byte, int64, error) {
	window := int64(ggufInitialHeaderBytes)
	for {
		data, total, err := fetchRange(ctx, opts, url, window)
		if err != nil {
			return nil, 0, err
		}
		if _, perr := parseGGUFHeader(data); errors.Is(perr, errShortHeader) && int64(len(data)) >= window {
			if window >= ggufMaxHeaderBytes {
				return nil, 0, fmt.Errorf("modelmeta: GGUF header exceeds %d bytes", ggufMaxHeaderBytes)
			}
			window *= 2
			continue
		}
		return data, total, nil
	}
}

func fetchRange(ctx context.Context, opts Options, url string, n int64) ([]byte, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("modelmeta: request header: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.client().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("modelmeta: fetch header: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		// Cap the read at the window even if the server ignored Range (200), so we
		// never pull the whole weights file.
		data, err := io.ReadAll(io.LimitReader(resp.Body, n))
		if err != nil {
			return nil, 0, err
		}
		return data, totalFromContentRange(resp.Header.Get("Content-Range")), nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, 0, fmt.Errorf("modelmeta: %s is gated or private — set HF_TOKEN (or HUGGING_FACE_HUB_TOKEN)", url)
	case http.StatusNotFound:
		return nil, 0, fmt.Errorf("modelmeta: %s not found", url)
	default:
		return nil, 0, fmt.Errorf("modelmeta: fetch header: unexpected status %s", resp.Status)
	}
}

// totalFromContentRange parses the total length from a Content-Range header of the
// form "bytes 0-1048575/4815319456"; returns 0 when the header is absent or its
// total is unknown ("*").
func totalFromContentRange(h string) int64 {
	i := strings.LastIndex(h, "/")
	if i < 0 {
		return 0
	}
	total, err := strconv.ParseInt(strings.TrimSpace(h[i+1:]), 10, 64)
	if err != nil {
		return 0
	}
	return total
}

func readLocalGGUFHeader(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("modelmeta: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, ggufInitialHeaderBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("modelmeta: read %s: %w", path, err)
	}
	return buf[:n], nil
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
