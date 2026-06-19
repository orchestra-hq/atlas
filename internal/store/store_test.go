package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// blobServer serves body at /blob and counts how many times it was fetched.
func blobServer(t *testing.T, body []byte) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPullVerifiesAndResolves(t *testing.T) {
	body := []byte("fake gguf weights")
	srv, hits := blobServer(t, body)
	s := New(t.TempDir())

	spec := PullSpec{
		Name:          "tiny-model",
		Engine:        "llamacpp",
		URL:           srv.URL + "/blob",
		SHA256:        sha256hex(body),
		ContextWindow: 4096,
	}
	m, err := s.Pull(context.Background(), spec)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if m.Digest != "sha256:"+spec.SHA256 || m.Size != int64(len(body)) {
		t.Errorf("manifest = %+v", m)
	}
	if !s.Has("tiny-model") {
		t.Error("Has = false after Pull")
	}

	path, err := s.Path("tiny-model")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("blob contents = %q", got)
	}
	if *hits != 1 {
		t.Errorf("server hit %d times, want 1", *hits)
	}
}

func TestPullIsIdempotent(t *testing.T) {
	body := []byte("weights v1")
	srv, hits := blobServer(t, body)
	s := New(t.TempDir())
	spec := PullSpec{Name: "m", Engine: "llamacpp", URL: srv.URL + "/blob", SHA256: sha256hex(body)}

	if _, err := s.Pull(context.Background(), spec); err != nil {
		t.Fatalf("first Pull: %v", err)
	}
	if _, err := s.Pull(context.Background(), spec); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if *hits != 1 {
		t.Errorf("server hit %d times, want 1 (second pull should skip)", *hits)
	}
}

func TestPullChecksumMismatch(t *testing.T) {
	body := []byte("real bytes")
	srv, _ := blobServer(t, body)
	s := New(t.TempDir())
	spec := PullSpec{Name: "m", URL: srv.URL + "/blob", SHA256: sha256hex([]byte("different"))}

	if _, err := s.Pull(context.Background(), spec); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	// A failed verify must not leave a usable model behind.
	if s.Has("m") {
		t.Error("Has = true after a checksum mismatch")
	}
}

func TestPullRequiresPinnedDigest(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Pull(context.Background(), PullSpec{Name: "m", URL: "http://example"}); err == nil {
		t.Fatal("expected error when sha256 is empty")
	}
}

func TestGetAbsent(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Get("nope"); err == nil {
		t.Fatal("expected error for absent model")
	}
	if s.Has("nope") {
		t.Error("Has = true for absent model")
	}
}

func TestInvalidNameRejected(t *testing.T) {
	s := New(t.TempDir())
	for _, bad := range []string{"../escape", "a/b", "has space", ""} {
		if _, err := s.Get(bad); err == nil {
			t.Errorf("Get(%q) = nil error, want rejection", bad)
		}
	}
}

func TestHasFalseWhenBlobMissing(t *testing.T) {
	body := []byte("blob")
	srv, _ := blobServer(t, body)
	s := New(t.TempDir())
	spec := PullSpec{Name: "m", URL: srv.URL + "/blob", SHA256: sha256hex(body)}
	if _, err := s.Pull(context.Background(), spec); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// Remove the blob but leave the manifest: a half-cleaned store reads absent.
	if err := os.Remove(s.blobPath("sha256:" + spec.SHA256)); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	if s.Has("m") {
		t.Error("Has = true with manifest present but blob gone")
	}
}

// A cached blob that no longer matches its recorded size (a truncated or
// partially-written file from an interrupted out-of-band copy) must read as
// absent so the caller re-pulls, rather than being handed to the engine as
// valid weights.
func TestTruncatedBlobReadsAbsentAndRepulls(t *testing.T) {
	body := []byte("the full set of weights")
	srv, hits := blobServer(t, body)
	s := New(t.TempDir())
	spec := PullSpec{Name: "m", Engine: "llamacpp", URL: srv.URL + "/blob", SHA256: sha256hex(body)}
	if _, err := s.Pull(context.Background(), spec); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("server hit %d times after first pull, want 1", *hits)
	}

	// Truncate the cached blob behind the store's back.
	if err := os.Truncate(s.blobPath("sha256:"+spec.SHA256), int64(len(body)-3)); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}

	if s.Has("m") {
		t.Error("Has = true with a truncated blob")
	}
	if _, err := s.Path("m"); err == nil {
		t.Error("Path = nil error with a truncated blob")
	}

	// Pull must not short-circuit on the corrupt blob; it re-downloads.
	if _, err := s.Pull(context.Background(), spec); err != nil {
		t.Fatalf("re-Pull: %v", err)
	}
	if *hits != 2 {
		t.Errorf("server hit %d times, want 2 (corrupt blob should re-download)", *hits)
	}
	if !s.Has("m") {
		t.Error("Has = false after re-pull repaired the blob")
	}
}

func TestProgressReported(t *testing.T) {
	body := []byte("0123456789")
	srv, _ := blobServer(t, body)
	var last int64
	s := New(t.TempDir())
	s.Progress = func(done, _ int64) { last = done }
	spec := PullSpec{Name: "m", URL: srv.URL + "/blob", SHA256: sha256hex(body)}
	if _, err := s.Pull(context.Background(), spec); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if last != int64(len(body)) {
		t.Errorf("final progress = %d, want %d", last, len(body))
	}
}
