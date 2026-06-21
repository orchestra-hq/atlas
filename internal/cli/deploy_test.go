package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/tlsx"
)

// TestNewAdminClientPinValidation covers the admin-side --tls-pin guardrails
// (M2 phase 1, ADR-0009): a malformed pin is rejected up front, and a pin against
// a plaintext http:// URL is a hard error rather than a silently unpinned
// connection. With no pin the client is the default (system trust).
func TestNewAdminClientPinValidation(t *testing.T) {
	t.Setenv("ATLAS_TLS_PIN", "") // isolate from the developer's environment

	if _, err := newAdminClient("https://server:9090", "k", "not-a-real-pin"); err == nil {
		t.Error("malformed pin accepted, want an error")
	}

	if _, err := newAdminClient("http://server:9090", "k", "sha256:"+strings.Repeat("ab", 32)); err == nil {
		t.Error("pin against an http:// URL accepted, want a hard error")
	}

	client, err := newAdminClient("https://server:9090", "k", "sha256:"+strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("valid pin against https:// rejected: %v", err)
	}
	if client.hc == http.DefaultClient {
		t.Error("pinned client is the default client; the pin was not applied")
	}

	unpinned, err := newAdminClient("http://server:9090", "k", "")
	if err != nil {
		t.Fatalf("unpinned client rejected: %v", err)
	}
	if unpinned.hc != http.DefaultClient {
		t.Error("unpinned client is not the default client")
	}
}

// TestAdminClientReachesSelfSignedGateway is the admin-side mirror of the worker's
// pinned-dial test: an admin command reaches an https:// gateway whose
// self-signed certificate matches the pin, with no OS trust-store install — the
// gap M1 left (the deploy command the --tls-self-signed banner printed failed
// against its own cert). A wrong pin must be refused.
func TestAdminClientReachesSelfSignedGateway(t *testing.T) {
	t.Setenv("ATLAS_TLS_PIN", "")

	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	pin := tlsx.Pin(srv.Certificate())
	client, err := newAdminClient(srv.URL, "admin-key", pin)
	if err != nil {
		t.Fatalf("newAdminClient: %v", err)
	}

	resp, err := client.do(context.Background(), http.MethodPost, "/admin/deployments", nil)
	if err != nil {
		t.Fatalf("pinned admin request to self-signed gateway failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if gotPath != "POST /admin/deployments" {
		t.Errorf("request = %q, want POST /admin/deployments", gotPath)
	}

	// A wrong (but well-formed) pin must not connect to the same server.
	wrongPin, _ := tlsx.NormalizePin(strings.Repeat("cd", 32))
	wrongClient, err := newAdminClient(srv.URL, "admin-key", wrongPin)
	if err != nil {
		t.Fatalf("newAdminClient (wrong pin): %v", err)
	}
	if _, err := wrongClient.do(context.Background(), http.MethodGet, "/admin/workers", nil); err == nil {
		t.Error("admin request with a wrong pin connected; the pin was not enforced")
	}
}
