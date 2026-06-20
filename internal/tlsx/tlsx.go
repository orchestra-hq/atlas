// Package tlsx holds the small TLS primitives shared by the Atlas server and
// worker for the M1 phase-7 transport-security model (ADR-0009): a certificate
// "pin" (the SHA-256 of the server's leaf certificate) that a worker can verify
// against, and a self-signed certificate generator for private deployments that
// have no public DNS name or CA. Public-CA / ACME certs need none of this — the
// worker trusts them through the system root store — so this package is only the
// self-signed-with-pinning path.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// pinPrefix labels a pin string with the hash it uses, so the format can evolve
// if SHA-256 ever needs replacing without silently reinterpreting old pins.
const pinPrefix = "sha256:"

// Pin returns the pin for a certificate: the SHA-256 of its raw DER bytes, hex
// encoded with a "sha256:" prefix. The server prints this for the cert it
// serves; a worker compares it against the cert the server actually presents.
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return pinPrefix + hex.EncodeToString(sum[:])
}

// NormalizePin canonicalizes an operator-supplied pin: it accepts the value
// with or without the "sha256:" prefix and in any case, and returns the
// canonical lower-case "sha256:<64 hex>" form. It errors on anything that is not
// a SHA-256 hex digest, so a malformed pin fails fast at startup rather than
// rejecting every connection at dial time.
func NormalizePin(s string) (string, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(strings.ToLower(raw), pinPrefix)
	if len(raw) != sha256.Size*2 {
		return "", fmt.Errorf("tls pin must be a sha256 hex digest (%d hex chars), got %d", sha256.Size*2, len(raw))
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return "", fmt.Errorf("tls pin is not valid hex: %w", err)
	}
	return pinPrefix + raw, nil
}

// PinnedVerifier returns a tls.Config VerifyConnection callback that accepts a
// connection iff the server's leaf certificate matches pin. It is meant to be
// paired with InsecureSkipVerify: true, which disables the default hostname and
// chain checks — appropriate here because a pinned self-signed cert is trusted
// by identity (its exact bytes), not by a CA chain or DNS name. pin must already
// be normalized (NormalizePin).
func PinnedVerifier(pin string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("tls: server presented no certificate")
		}
		got := Pin(cs.PeerCertificates[0])
		if got != pin {
			return fmt.Errorf("tls: server certificate pin %s does not match expected %s", got, pin)
		}
		return nil
	}
}

// SelfSigned is a generated self-signed certificate and the PEM bytes to persist
// it, plus its pin for the operator to copy to workers.
type SelfSigned struct {
	Cert    tls.Certificate
	CertPEM []byte
	KeyPEM  []byte
	Pin     string
}

// GenerateSelfSigned creates a self-signed ECDSA P-256 certificate valid for the
// given hosts (DNS names and/or IPs; loopback names are added so a local worker
// can always connect). It is used by `atlas server --tls-self-signed` for
// private deployments without a public CA. The returned PEM bytes are written to
// the state dir and reloaded on restart so the pin is stable across restarts.
func GenerateSelfSigned(hosts []string) (SelfSigned, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return SelfSigned{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return SelfSigned{}, fmt.Errorf("generate serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "atlas"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0), // long-lived: identity is the pin, not expiry
		// A leaf server certificate, not a CA: the worker trusts it by pinning its
		// exact bytes (PinnedVerifier), so it never needs to sign other certs. Minting
		// it without IsCA/CertSign keeps the on-disk private key from doubling as a
		// signing CA if it ever leaks or an operator installs the cert into a trust store.
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	for _, h := range dedupeHosts(hosts) {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return SelfSigned{}, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return SelfSigned{}, fmt.Errorf("marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return SelfSigned{}, fmt.Errorf("load generated keypair: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return SelfSigned{}, fmt.Errorf("parse generated certificate: %w", err)
	}
	return SelfSigned{Cert: cert, CertPEM: certPEM, KeyPEM: keyPEM, Pin: Pin(leaf)}, nil
}

// PinForCertPEM returns the pin of the leaf certificate in a PEM bundle, so the
// server can print the pin for an operator-supplied --tls-cert too.
func PinForCertPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("tls: no PEM certificate found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("tls: parse certificate: %w", err)
	}
	return Pin(cert), nil
}

// dedupeHosts removes empty and duplicate hosts while preserving order, always
// including loopback so a co-located worker can connect by 127.0.0.1/localhost.
func dedupeHosts(hosts []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts)+2)
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	for _, h := range hosts {
		add(h)
	}
	add("127.0.0.1")
	add("localhost")
	return out
}
