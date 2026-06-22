package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
)

// --- unit tests ------------------------------------------------------------

func anthErr(status int) *anthropic.Error { return &anthropic.Error{Status: status} }

// TestShouldSpill: spill triggers only on a would-shed (429/529) or
// unavailable-but-mappable (404) status, only for a configured model, and never when
// disabled.
func TestShouldSpill(t *testing.T) {
	c := NewCloudFallback(map[string]CloudTarget{
		"m1": {Provider: CloudProviderAnthropic, BaseURL: "http://x", Model: "up", APIKey: "k"},
	}, nil)

	for _, status := range []int{http.StatusTooManyRequests, statusOverloaded, http.StatusNotFound} {
		if !c.shouldSpill("m1", anthErr(status)) {
			t.Errorf("status %d on a configured model should spill", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		if c.shouldSpill("m1", anthErr(status)) {
			t.Errorf("status %d should not spill (real client error / success)", status)
		}
	}
	if c.shouldSpill("other-model", anthErr(429)) {
		t.Error("a model with no target should not spill")
	}
	if c.shouldSpill("m1", nil) {
		t.Error("nil error should not spill")
	}
	// Disabled (nil controller, or no targets) never spills.
	var nilC *CloudFallback
	if nilC.shouldSpill("m1", anthErr(429)) {
		t.Error("nil controller should not spill")
	}
	if NewCloudFallback(nil, nil).shouldSpill("m1", anthErr(429)) {
		t.Error("controller with no targets should not spill")
	}
}

// TestRewriteModelField: the model is replaced and every other field is preserved.
func TestRewriteModelField(t *testing.T) {
	in := `{"model":"local","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	out, err := rewriteModelField([]byte(in), "upstream-model")
	if err != nil {
		t.Fatalf("rewriteModelField: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if m["model"] != "upstream-model" {
		t.Fatalf("model = %v, want upstream-model", m["model"])
	}
	if m["max_tokens"].(float64) != 64 || m["stream"] != true {
		t.Fatalf("other fields not preserved: %v", m)
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Fatalf("messages not preserved: %v", m["messages"])
	}
}

// TestTokenSniffer_buffered parses usage from a single buffered JSON body, both
// Anthropic and OpenAI shaped.
func TestTokenSniffer_buffered(t *testing.T) {
	anth := &tokenSniffer{}
	_, _ = anth.Write([]byte(`{"type":"message","usage":{"input_tokens":11,"output_tokens":22}}`))
	if anth.input != 11 || anth.output != 22 {
		t.Fatalf("anthropic sniff = (%d,%d), want (11,22)", anth.input, anth.output)
	}
	oai := &tokenSniffer{}
	_, _ = oai.Write([]byte(`{"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}`))
	if oai.input != 7 || oai.output != 9 {
		t.Fatalf("openai sniff = (%d,%d), want (7,9)", oai.input, oai.output)
	}
}

// TestTokenSniffer_streamingChunks: across many small writes (an SSE stream), the
// sniffer keeps the last output value — the final cumulative total — even when a
// field is split across a chunk boundary.
func TestTokenSniffer_streamingChunks(t *testing.T) {
	s := &tokenSniffer{}
	// input_tokens arrives early; output_tokens accumulates across message_delta events.
	stream := []string{
		`event: message_start` + "\n" + `data: {"message":{"usage":{"input_tokens":5,"out`,
		`put_tokens":1}}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"usage":{"output_tokens":12}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"usage":{"output_tokens":40}}` + "\n\n",
	}
	for _, chunk := range stream {
		_, _ = s.Write([]byte(chunk))
	}
	if s.input != 5 {
		t.Fatalf("input = %d, want 5 (split across a chunk boundary)", s.input)
	}
	if s.output != 40 {
		t.Fatalf("output = %d, want 40 (last cumulative value)", s.output)
	}
}

// TestSetCloudAuth: each provider gets its own auth header style.
func TestSetCloudAuth(t *testing.T) {
	ar, _ := http.NewRequest("POST", "http://x", nil)
	setCloudAuth(ar, CloudTarget{Provider: CloudProviderAnthropic, APIKey: "sk-ant"})
	if ar.Header.Get("x-api-key") != "sk-ant" || ar.Header.Get("anthropic-version") == "" {
		t.Fatalf("anthropic auth headers = %v", ar.Header)
	}
	or, _ := http.NewRequest("POST", "http://x", nil)
	setCloudAuth(or, CloudTarget{Provider: CloudProviderOpenAI, APIKey: "sk-oai"})
	if or.Header.Get("authorization") != "Bearer sk-oai" {
		t.Fatalf("openai auth header = %q", or.Header.Get("authorization"))
	}
}

// --- integration (G22) -----------------------------------------------------

// cloudServer builds a gateway with no local models, a cloud target for "spill-model"
// pointing at upstreamURL, plus a usage recorder and metrics, served over HTTP. With
// no local route, a request to "spill-model" resolves to a 404 (unavailable-but-
// mappable), which is the same spill path overflow's 429/529 takes.
func cloudServer(t *testing.T, upstreamURL, provider string) (*httptest.Server, *recordingUsage, *Metrics) {
	t.Helper()
	rec := &recordingUsage{}
	g := NewGateway(staticAuth(testKey), nil, nil)
	m := NewMetrics()
	g.SetMetrics(m)
	g.SetUsageRecorder(rec)
	g.SetCloudFallback(NewCloudFallback(map[string]CloudTarget{
		"spill-model": {Provider: provider, BaseURL: upstreamURL, Model: "upstream-model", APIKey: "sk-test"},
	}, nil))
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return srv, rec, m
}

// TestCloudFallback_spillBuffered: an unmapped local model spills to the upstream,
// the response is labeled x-atlas-served-by: cloud with the body relayed unchanged,
// and the tokens are attributed to the cloud usage class.
func TestCloudFallback_spillBuffered(t *testing.T) {
	var gotModel, gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %s, want /v1/messages", r.URL.Path)
		}
		gotKey = r.Header.Get("x-api-key")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_up","type":"message","content":[{"type":"text","text":"from cloud"}],"usage":{"input_tokens":11,"output_tokens":22}}`))
	}))
	defer upstream.Close()

	srv, rec, m := cloudServer(t, upstream.URL, CloudProviderAnthropic)
	resp, body := post(t, srv, testKey, `{"model":"spill-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	if got := resp.Header.Get(servedByHeader); got != servedByCloud {
		t.Fatalf("%s = %q, want cloud", servedByHeader, got)
	}
	if content, _ := body["content"].([]any); len(content) == 0 {
		t.Fatalf("relayed body missing upstream content: %v", body)
	}
	if gotModel != "upstream-model" || gotKey != "sk-test" {
		t.Fatalf("upstream saw model=%q key=%q, want upstream-model/sk-test", gotModel, gotKey)
	}

	got := rec.waitForRecord(t)
	if r := got[0]; r.WorkerID != "cloud:anthropic" || r.Model != "spill-model" || r.InputTokens != 11 || r.OutputTokens != 22 {
		t.Fatalf("cloud usage = %+v, want worker=cloud:anthropic model=spill-model tokens=(11,22)", r)
	}
	if snap := m.Snapshot(); snap.CloudSpills != 1 {
		t.Fatalf("cloud spill metric = %d, want 1", snap.CloudSpills)
	}
}

// TestCloudFallback_spillStreaming: a streaming spill relays the SSE response and
// records usage sniffed from the stream.
func TestCloudFallback_spillStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":8}}}` + "\n\n",
			`data: {"type":"content_block_delta","delta":{"text":"hello"}}` + "\n\n",
			`data: {"type":"message_delta","usage":{"output_tokens":33}}` + "\n\n",
		} {
			_, _ = w.Write([]byte(line))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	srv, rec, _ := cloudServer(t, upstream.URL, CloudProviderAnthropic)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{"model":"spill-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get(servedByHeader) != servedByCloud {
		t.Fatalf("%s = %q, want cloud", servedByHeader, resp.Header.Get(servedByHeader))
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "message_start") {
		t.Fatalf("relayed stream missing upstream events: %q", buf[:n])
	}
	// Drain the rest so usage is sniffed and recorded.
	_, _ = io.ReadAll(resp.Body)

	got := rec.waitForRecord(t)
	if r := got[0]; r.WorkerID != "cloud:anthropic" || r.InputTokens != 8 || r.OutputTokens != 33 {
		t.Fatalf("cloud streaming usage = %+v, want worker=cloud:anthropic tokens=(8,33)", r)
	}
}

// TestCloudFallback_disabledSheds: with no target configured, the same unmapped model
// sheds its original error unchanged and is never labeled cloud.
func TestCloudFallback_disabledSheds(t *testing.T) {
	g := NewGateway(staticAuth(testKey), nil, nil)
	g.SetCloudFallback(NewCloudFallback(nil, nil)) // off
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	resp, _ := post(t, srv, testKey, `{"model":"spill-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unchanged shed)", resp.StatusCode)
	}
	if resp.Header.Get(servedByHeader) == servedByCloud {
		t.Fatal("a non-spilled response must not be labeled cloud")
	}
}

// TestCloudFallback_upstreamUnreachable: when the upstream cannot be reached, the
// spill surfaces a clean retryable overload (529), not a hang, and no cloud label.
func TestCloudFallback_upstreamUnreachable(t *testing.T) {
	// A target pointing at a closed listener address.
	srv, _, _ := cloudServer(t, "http://127.0.0.1:1", CloudProviderAnthropic)
	resp, _ := post(t, srv, testKey, `{"model":"spill-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != statusOverloaded {
		t.Fatalf("status = %d, want 529 (clean overload on unreachable upstream)", resp.StatusCode)
	}
	if resp.Header.Get(servedByHeader) == servedByCloud {
		t.Fatal("an unreached upstream must not be labeled cloud")
	}
}

// TestServedByLocalHeader: a normally served local response is labeled local.
func TestServedByLocalHeader(t *testing.T) {
	srv := newTestServer(&echoExecutor{reply: "hi"})
	resp, _ := post(t, srv, testKey, `{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if got := resp.Header.Get(servedByHeader); got != servedByLocal {
		t.Fatalf("%s = %q, want local", servedByHeader, got)
	}
}
