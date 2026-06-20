package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
)

func TestNormalizePin(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"bare hex", hex64, "sha256:" + hex64, false},
		{"with prefix", "sha256:" + hex64, "sha256:" + hex64, false},
		{"uppercase", strings.ToUpper(hex64), "sha256:" + hex64, false},
		{"whitespace", "  " + hex64 + "  ", "sha256:" + hex64, false},
		{"too short", "abcd", "", true},
		{"not hex", strings.Repeat("z", 64), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizePin(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("NormalizePin(%q) = %q, want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizePin(%q) errored: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("NormalizePin(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestGenerateSelfSignedAndPin(t *testing.T) {
	gen, err := GenerateSelfSigned([]string{"atlas.example"})
	if err != nil {
		t.Fatal(err)
	}

	// The keypair is loadable and the pin matches the leaf the server will serve.
	leaf, err := x509.ParseCertificate(gen.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := Pin(leaf); got != gen.Pin {
		t.Errorf("Pin(leaf) = %q, want reported pin %q", got, gen.Pin)
	}
	if !strings.HasPrefix(gen.Pin, "sha256:") || len(gen.Pin) != len("sha256:")+64 {
		t.Errorf("pin %q is not sha256:<64 hex>", gen.Pin)
	}

	// Loopback is always present so a co-located worker can connect; the explicit
	// host is included too.
	if err := leaf.VerifyHostname("atlas.example"); err != nil {
		t.Errorf("cert does not cover the requested host atlas.example: %v", err)
	}
	hasLoopback := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Error("cert does not include the 127.0.0.1 loopback SAN")
	}

	// The cert is a leaf server certificate, not a CA: pinning trusts its exact
	// bytes, so it must not carry CA/cert-signing privilege that would let the
	// on-disk key sign other certificates if it leaked.
	if leaf.IsCA {
		t.Error("self-signed leaf is marked IsCA; a served leaf must not be a CA")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("self-signed leaf has KeyUsageCertSign; a served leaf must not sign certificates")
	}
}

func TestPinnedVerifier(t *testing.T) {
	gen, err := GenerateSelfSigned(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(gen.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// Matching pin: accepted.
	if err := PinnedVerifier(gen.Pin)(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Errorf("matching pin rejected: %v", err)
	}
	// Wrong pin: rejected.
	wrong, _ := NormalizePin(strings.Repeat("bc", 32))
	if err := PinnedVerifier(wrong)(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
		t.Error("wrong pin was accepted")
	}
	// No certificate presented: rejected.
	if err := PinnedVerifier(gen.Pin)(tls.ConnectionState{}); err == nil {
		t.Error("empty peer certificate chain was accepted")
	}
}

func TestPinForCertPEM(t *testing.T) {
	gen, err := GenerateSelfSigned(nil)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := PinForCertPEM(gen.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if pin != gen.Pin {
		t.Errorf("PinForCertPEM = %q, want %q", pin, gen.Pin)
	}
	if _, err := PinForCertPEM([]byte("not a pem")); err == nil {
		t.Error("PinForCertPEM accepted non-PEM input")
	}
}
