package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/core"
)

// fakeEmbedder is an executor that serves the embedding class: Embed returns fixed
// vectors, while Execute errors (an embedding engine does not do chat) so a test
// that wrongly routes a chat request here would fail loudly rather than silently
// pass.
type fakeEmbedder struct {
	vecs     [][]float32
	inTokens int
	gotInput []string
	err      error
}

func (f *fakeEmbedder) Execute(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errors.New("embedding model cannot serve chat")
}

func (f *fakeEmbedder) Embed(_ context.Context, req core.EmbedRequest) (core.EmbedResponse, error) {
	f.gotInput = req.Input
	if f.err != nil {
		return core.EmbedResponse{}, f.err
	}
	return core.EmbedResponse{Embeddings: f.vecs, Usage: core.Usage{InputTokens: f.inTokens}}, nil
}

// embeddingsServer builds a gateway serving the given models and returns an HTTP
// test server over its handler.
func embeddingsServer(models ...Model) *httptest.Server {
	g := NewGateway(staticAuth(testKey), models, nil)
	return httptest.NewServer(g.Handler())
}

func embedPost(t *testing.T, srv *httptest.Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	_ = resp.Body.Close()
	return resp, decoded
}

// TestEmbeddings_roundTrip is the G20 embeddings happy path: a deployed
// embedding-class model serves POST /v1/embeddings in the OpenAI shape, returning
// one vector per input in order with token usage.
func TestEmbeddings_roundTrip(t *testing.T) {
	emb := &fakeEmbedder{vecs: [][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}}, inTokens: 11}
	srv := embeddingsServer(Model{Name: "embed-model", Exec: emb, Class: catalog.ClassEmbedding})
	defer srv.Close()

	resp, body := embedPost(t, srv, `{"model":"embed-model","input":["hello","world"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	if body["object"] != "list" {
		t.Fatalf("object = %v, want list", body["object"])
	}
	if body["model"] != "embed-model" {
		t.Fatalf("model echo = %v, want embed-model", body["model"])
	}
	data, _ := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data length = %d, want 2", len(data))
	}
	first, _ := data[0].(map[string]any)
	if first["object"] != "embedding" || first["index"].(float64) != 0 {
		t.Fatalf("first datum shape wrong: %v", first)
	}
	vec, _ := first["embedding"].([]any)
	if len(vec) != 3 || vec[0].(float64) != 0.1 {
		t.Fatalf("first vector = %v, want [0.1 0.2 0.3]", vec)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 11 {
		t.Fatalf("usage.prompt_tokens = %v, want 11", usage["prompt_tokens"])
	}
	if strings.Join(emb.gotInput, ",") != "hello,world" {
		t.Fatalf("engine saw inputs %v, want [hello world]", emb.gotInput)
	}
}

// TestEmbeddings_singleStringInput: the OpenAI SDK also sends input as a bare
// string, which must normalize to a one-element batch.
func TestEmbeddings_singleStringInput(t *testing.T) {
	emb := &fakeEmbedder{vecs: [][]float32{{1, 2}}}
	srv := embeddingsServer(Model{Name: "embed-model", Exec: emb, Class: catalog.ClassEmbedding})
	defer srv.Close()

	resp, body := embedPost(t, srv, `{"model":"embed-model","input":"just one"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	if got := strings.Join(emb.gotInput, "|"); got != "just one" {
		t.Fatalf("engine saw %q, want single input", got)
	}
}

// TestEmbeddings_wrongClassChatModel is the G20 rejection case: an embeddings
// request against a chat model is rejected with a clean 400, not a 5xx, and the
// chat executor is never invoked.
func TestEmbeddings_wrongClassChatModel(t *testing.T) {
	chat := &echoExecutor{reply: "should never run"}
	srv := embeddingsServer(Model{Name: "chat-model", Exec: chat, Class: catalog.ClassChat})
	defer srv.Close()

	resp, body := embedPost(t, srv, `{"model":"chat-model","input":["x"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for wrong-class request (body %v)", resp.StatusCode, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "invalid_request_error" {
		t.Fatalf("error envelope = %v, want invalid_request_error", body)
	}
}

// TestChat_wrongClassEmbeddingModel: the inverse — a chat request against an
// embedding model is rejected with a clean 400 on the Anthropic surface.
func TestChat_wrongClassEmbeddingModel(t *testing.T) {
	emb := &fakeEmbedder{vecs: [][]float32{{1}}}
	srv := embeddingsServer(Model{Name: "embed-model", Exec: emb, Class: catalog.ClassEmbedding})
	defer srv.Close()

	resp, body := post(t, srv, testKey, `{"model":"embed-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for chat-on-embedding-model (body %v)", resp.StatusCode, body)
	}
}

// TestEmbeddings_unknownModel: an unknown model is a 404, distinct from the
// wrong-class 400.
func TestEmbeddings_unknownModel(t *testing.T) {
	srv := embeddingsServer()
	defer srv.Close()

	resp, _ := embedPost(t, srv, `{"model":"nope","input":["x"]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown model", resp.StatusCode)
	}
}

// TestEmbeddings_validationErrors: missing model or input is a clean 400 before any
// dispatch.
func TestEmbeddings_validationErrors(t *testing.T) {
	emb := &fakeEmbedder{vecs: [][]float32{{1}}}
	srv := embeddingsServer(Model{Name: "embed-model", Exec: emb, Class: catalog.ClassEmbedding})
	defer srv.Close()

	for _, body := range []string{`{"input":["x"]}`, `{"model":"embed-model"}`, `{"model":"embed-model","input":[]}`} {
		resp, _ := embedPost(t, srv, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestRouteClass_defaultsToChat: a route registered with no class reads back as
// chat, so a pre-class model routes to the chat surfaces unchanged.
func TestRouteClass_defaultsToChat(t *testing.T) {
	g := NewGateway(staticAuth(testKey), []Model{{Name: "m", Exec: &echoExecutor{}}}, nil)
	cls, known := g.routeClass("m")
	if !known || cls != catalog.ClassChat {
		t.Fatalf("routeClass = (%q,%v), want (chat,true)", cls, known)
	}
	if _, known := g.routeClass("absent"); known {
		t.Fatal("routeClass for an unrouted model should be known=false")
	}
}
