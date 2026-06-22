package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
)

// servedByHeader labels every chat response with where it was served (M3 phase 4,
// ADR-0013): "local" for the fleet, "cloud" for a cloud-fallback spill. It is the
// out-of-band signal that keeps the drop-in promise — the response body is untouched,
// so the SDK parses it identically; only operators and dashboards read this header.
const (
	servedByHeader = "x-atlas-served-by"
	servedByLocal  = "local"
	servedByCloud  = "cloud"
)

// Cloud providers Atlas can spill to. The value selects the auth header style; the
// request path (and thus the wire shape) comes from the surface the client used.
const (
	CloudProviderAnthropic = "anthropic"
	CloudProviderOpenAI    = "openai"
)

// CloudTarget is the upstream a model spills to when the local fleet would shed it
// (ADR-0013). Provider selects the auth style, BaseURL the host, Model the upstream
// model name the local name maps to, APIKey the operator-supplied credential.
type CloudTarget struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
}

// CloudFallback spills overflow to a real provider instead of shedding it (ADR-0013).
// It is opt-in and off by default: a nil controller, or one with no configured
// targets, makes every shed stay a shed — ADR-0010 behavior, no request leaves the
// operator's infrastructure. Targets are keyed by the requested model name, so
// fallback is enabled per model with explicit upstream credentials. All methods are
// safe on a nil receiver.
type CloudFallback struct {
	targets map[string]CloudTarget
	http    *http.Client
	metrics *Metrics
	logger  *slog.Logger
}

// NewCloudFallback builds a controller from per-model upstream targets. An empty map
// returns nil — fallback off — so the gateway treats "no targets configured" and "no
// controller" identically.
func NewCloudFallback(targets map[string]CloudTarget, hc *http.Client) *CloudFallback {
	if len(targets) == 0 {
		return nil
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &CloudFallback{targets: targets, http: hc, logger: slog.Default()}
}

// enabled reports whether any model has a fallback target.
func (c *CloudFallback) enabled() bool { return c != nil && len(c.targets) > 0 }

// target returns the upstream configured for a requested model, if fallback is
// enabled for it.
func (c *CloudFallback) target(model string) (CloudTarget, bool) {
	if !c.enabled() {
		return CloudTarget{}, false
	}
	t, ok := c.targets[model]
	return t, ok
}

// shouldSpill reports whether a local dispatch failure should spill to the cloud
// rather than surface to the client. It triggers only on the two ADR-0013 cases —
// overflow that ADR-0010 would shed (429/529) or an unavailable-but-mappable model
// (404) — and only when a target is configured for the model. A 400 (oversized
// prompt), 401, or 403 is a real client error and never spills.
func (c *CloudFallback) shouldSpill(model string, apiErr *anthropic.Error) bool {
	if apiErr == nil || !c.enabled() {
		return false
	}
	switch apiErr.Status {
	case http.StatusTooManyRequests, statusOverloaded, http.StatusNotFound:
		_, ok := c.target(model)
		return ok
	default:
		return false
	}
}

// spillToCloud forwards the request to the model's upstream target and relays the
// response to the client, labeling it x-atlas-served-by: cloud and attributing its
// tokens to the cloud usage class (M3 phase 4). The request path is reused, so an
// Anthropic /v1/messages request spills to the upstream's /v1/messages and an OpenAI
// /v1/chat/completions to its /v1/chat/completions; only the model field and the auth
// header change, so the upstream response is byte-for-byte what the SDK expects.
//
// It returns nil once it has relayed a response (including a provider error response,
// which is a normal error the SDK handles). It returns an error only when the upstream
// could not be reached at all and nothing was written, so the caller can surface a
// clean, surface-appropriate error instead of a hang (ADR-0013 consequence: a failed
// upstream call is a normal error). keyID attributes the cloud usage to the calling key.
func (g *Gateway) spillToCloud(w http.ResponseWriter, r *http.Request, body []byte, requested, keyID string) error {
	c := g.cloud
	t, ok := c.target(requested)
	if !ok {
		return fmt.Errorf("cloud fallback: no target for %q", requested)
	}

	upstreamBody, err := rewriteModelField(body, t.Model)
	if err != nil {
		return fmt.Errorf("cloud fallback: rewrite request: %w", err)
	}

	url := trimTrailingSlash(t.BaseURL) + r.URL.Path
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(upstreamBody))
	if err != nil {
		return fmt.Errorf("cloud fallback: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	setCloudAuth(req, t)

	resp, err := c.http.Do(req)
	if err != nil {
		// Upstream unreachable: nothing written yet, so the caller surfaces a clean
		// retryable error. Counted as a spill error so the metric does not under-report.
		c.metrics.incCloudSpill(requested, t.Provider, "error")
		if c.logger != nil {
			c.logger.Warn("cloud fallback upstream unreachable", "model", requested, "provider", t.Provider, "error", err)
		}
		return fmt.Errorf("cloud fallback: call upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Relay: label out-of-band, copy the upstream content-type and status, then stream
	// the body through unchanged (works for both buffered JSON and SSE).
	w.Header().Set(servedByHeader, servedByCloud)
	if ct := resp.Header.Get("content-type"); ct != "" {
		w.Header().Set("content-type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	sniff := &tokenSniffer{}
	_ = copyFlush(w, io.TeeReader(resp.Body, sniff))

	c.metrics.incCloudSpill(requested, t.Provider, cloudResultLabel(resp.StatusCode))
	// Attribute the spend to the cloud usage class only on a successful response;
	// a provider error carries no billable usage. workerID "cloud:<provider>" makes
	// the cloud ledger distinct from local serving in `atlas usage` and /metrics.
	if resp.StatusCode < http.StatusBadRequest {
		recordBillableUsage(r.Context(), usageTags{
			keyID:    keyID,
			workerID: "cloud:" + t.Provider,
			model:    requested,
		}, sniff.input, sniff.output)
	}
	return nil
}

// setCloudAuth sets the upstream provider's authentication header on the forwarded
// request, replacing the client's Atlas key with the operator's provider key.
func setCloudAuth(req *http.Request, t CloudTarget) {
	switch t.Provider {
	case CloudProviderAnthropic:
		req.Header.Set("x-api-key", t.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	case CloudProviderOpenAI:
		req.Header.Set("authorization", "Bearer "+t.APIKey)
	}
}

// rewriteModelField replaces the JSON body's "model" with the upstream model name,
// leaving every other field's bytes untouched (a near-identity passthrough — the
// request is already in the provider's wire shape). It decodes into raw messages so
// nested structures are not re-encoded.
func rewriteModelField(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = mb
	return json.Marshal(m)
}

// copyFlush streams r to w, flushing after each chunk so a Server-Sent-Events relay
// reaches the client incrementally rather than buffering to EOF. A write error stops
// the copy (the client went away); the upstream read continuing to EOF is the caller's
// deferred Close.
func copyFlush(w http.ResponseWriter, r io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// cloudResultLabel maps an upstream HTTP status to a spill-metric result label.
func cloudResultLabel(status int) string {
	if status < http.StatusBadRequest {
		return "served"
	}
	return "upstream_error"
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// tokenTokenRE matches the usage token fields of both the Anthropic and OpenAI
// response shapes, buffered or streamed, so one sniffer covers every spill.
var tokenTokenRE = regexp.MustCompile(`"(input_tokens|output_tokens|prompt_tokens|completion_tokens)"\s*:\s*(\d+)`)

// tokenSniffer extracts usage token counts from a relayed response without buffering
// it (M3 phase 4): it is tee'd off the byte stream as it flows to the client and
// scans for the providers' usage fields, keeping the last value seen for each — which
// is the final total for a streamed response, where output tokens accumulate across
// deltas. A small carry between writes catches a field split across chunk boundaries.
// It is best-effort: a response with no usage fields records zero, never an error.
type tokenSniffer struct {
	input  int
	output int
	carry  []byte
}

// tokenCarry is how many trailing bytes to re-scan on the next write so a usage field
// straddling a chunk boundary is still matched. Comfortably exceeds the longest field
// (`"completion_tokens": <digits>`).
const tokenCarry = 64

func (s *tokenSniffer) Write(p []byte) (int, error) {
	data := p
	if len(s.carry) > 0 {
		data = make([]byte, 0, len(s.carry)+len(p))
		data = append(data, s.carry...)
		data = append(data, p...)
	}
	for _, m := range tokenTokenRE.FindAllSubmatch(data, -1) {
		n, err := strconv.Atoi(string(m[2]))
		if err != nil {
			continue
		}
		switch string(m[1]) {
		case "input_tokens", "prompt_tokens":
			s.input = n
		case "output_tokens", "completion_tokens":
			s.output = n
		}
	}
	if len(data) > tokenCarry {
		s.carry = append(s.carry[:0], data[len(data)-tokenCarry:]...)
	} else {
		s.carry = append(s.carry[:0], data...)
	}
	return len(p), nil
}
