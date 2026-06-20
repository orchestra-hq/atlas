package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/orchestra-hq/atlas/internal/tlsx"
)

func TestResolveServerTLS_plaintextDefault(t *testing.T) {
	res, err := resolveServerTLS(tlsOptions{}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.config != nil || res.scheme != "http" {
		t.Errorf("default = %+v, want plaintext http with nil config", res)
	}
}

func TestResolveServerTLS_mutuallyExclusive(t *testing.T) {
	_, err := resolveServerTLS(tlsOptions{selfSigned: true, acmeDomain: "x.example"}, t.TempDir(), nil)
	if err == nil {
		t.Fatal("two TLS modes were accepted; want an error")
	}
}

func TestResolveServerTLS_selfSignedStablePin(t *testing.T) {
	dir := t.TempDir()

	first, err := resolveServerTLS(tlsOptions{selfSigned: true}, dir, []string{"host.example"})
	if err != nil {
		t.Fatal(err)
	}
	if first.config == nil || first.scheme != "https" || first.pin == "" {
		t.Fatalf("self-signed result = %+v, want https + a pin + config", first)
	}
	// Cert/key were persisted.
	if _, err := os.Stat(filepath.Join(dir, "tls", "cert.pem")); err != nil {
		t.Errorf("cert not persisted: %v", err)
	}

	// A second resolve reuses the cached material: the pin is stable across
	// restarts, which is what lets a worker keep a fixed --tls-pin.
	second, err := resolveServerTLS(tlsOptions{selfSigned: true}, dir, []string{"host.example"})
	if err != nil {
		t.Fatal(err)
	}
	if second.pin != first.pin {
		t.Errorf("pin changed across restarts: %q then %q", first.pin, second.pin)
	}
}

func TestResolveServerTLS_providedCert(t *testing.T) {
	dir := t.TempDir()
	gen, err := tlsx.GenerateSelfSigned([]string{"provided.example"})
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certPath, gen.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, gen.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := resolveServerTLS(tlsOptions{certFile: certPath, keyFile: keyPath}, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.scheme != "https" || res.config == nil {
		t.Fatalf("provided-cert result = %+v, want https + config", res)
	}
	if res.pin != gen.Pin {
		t.Errorf("provided-cert pin = %q, want %q", res.pin, gen.Pin)
	}

	// A cert without its key is a usage error, not a silent plaintext fallback.
	if _, err := resolveServerTLS(tlsOptions{certFile: certPath}, dir, nil); err == nil {
		t.Error("--tls-cert without --tls-key was accepted")
	}
}

func TestResolveServerTLS_acmeConfigured(t *testing.T) {
	res, err := resolveServerTLS(tlsOptions{acmeDomain: "atlas.example"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// ACME yields a live config but no pin (workers trust the public chain).
	if res.config == nil || res.scheme != "https" {
		t.Fatalf("acme result = %+v, want https + config", res)
	}
	if res.pin != "" {
		t.Errorf("acme pin = %q, want empty", res.pin)
	}
}

func TestSelfSignedHosts(t *testing.T) {
	// A wildcard listen host contributes no SAN.
	if hosts := selfSignedHosts("0.0.0.0"); slices.Contains(hosts, "0.0.0.0") {
		t.Errorf("0.0.0.0 should not be a SAN, got %v", hosts)
	}
	// A concrete listen host is included.
	if hosts := selfSignedHosts("10.0.0.5"); !slices.Contains(hosts, "10.0.0.5") {
		t.Errorf("concrete host missing from SANs: %v", hosts)
	}
}
