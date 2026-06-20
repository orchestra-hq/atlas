package cli

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/acme/autocert"

	"github.com/orchestra-hq/atlas/internal/tlsx"
)

// tlsOptions are the mutually exclusive `atlas server` TLS modes (ADR-0009).
type tlsOptions struct {
	certFile   string // operator-supplied cert (PEM); paired with keyFile
	keyFile    string
	selfSigned bool   // generate (and cache) a self-signed cert; print its pin
	acmeDomain string // Let's Encrypt for this public DNS name (needs :443)
	acmeEmail  string // optional ACME account email
}

// tlsResult is the outcome of resolving TLS options: the server's tls.Config
// (nil = serve plaintext), the cert pin workers use for a self-signed/provided
// cert (empty for ACME, where workers trust the public chain), the URL scheme,
// and banner lines describing the mode.
type tlsResult struct {
	config *tls.Config
	pin    string
	scheme string // "http" or "https"
	notes  []string
}

// selfSignedHosts derives SAN hosts for a generated self-signed cert from the
// listen host and this machine's hostname; loopback names are added by tlsx. A
// wildcard or empty listen host contributes no SAN (clients reach it by some
// concrete name the operator knows, and pinned workers skip the name check
// anyway).
func selfSignedHosts(listenHost string) []string {
	var hosts []string
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	switch listenHost {
	case "", "0.0.0.0", "::", "[::]":
	default:
		hosts = append(hosts, listenHost)
	}
	return hosts
}

// resolveServerTLS turns the chosen TLS mode into a tls.Config. The modes are
// mutually exclusive; none selected serves plaintext (the dev/internal default,
// ADR-0007). hosts are extra SANs for a generated self-signed cert (loopback is
// always added). Self-signed material is cached under stateDir so the pin is
// stable across restarts.
func resolveServerTLS(opts tlsOptions, stateDir string, hosts []string) (tlsResult, error) {
	modes := 0
	for _, on := range []bool{opts.certFile != "" || opts.keyFile != "", opts.selfSigned, opts.acmeDomain != ""} {
		if on {
			modes++
		}
	}
	if modes == 0 {
		return tlsResult{scheme: "http"}, nil
	}
	if modes > 1 {
		return tlsResult{}, fmt.Errorf("choose one TLS mode: --tls-cert/--tls-key, --tls-self-signed, or --tls-acme-domain")
	}

	switch {
	case opts.acmeDomain != "":
		return acmeTLS(opts, stateDir), nil
	case opts.certFile != "" || opts.keyFile != "":
		return providedCertTLS(opts)
	default:
		return selfSignedTLS(stateDir, hosts)
	}
}

// acmeTLS configures Let's Encrypt via autocert. The certificate is obtained on
// the first TLS handshake (TLS-ALPN-01), so the server must be reachable on
// :443 from the public internet for the domain to validate.
func acmeTLS(opts tlsOptions, stateDir string) tlsResult {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(opts.acmeDomain),
		Cache:      autocert.DirCache(filepath.Join(stateDir, "acme")),
		Email:      opts.acmeEmail,
	}
	return tlsResult{
		config: m.TLSConfig(),
		scheme: "https",
		notes: []string{
			fmt.Sprintf("TLS     : ACME (Let's Encrypt) for %s", opts.acmeDomain),
			"          the server must be reachable on :443 for the domain to validate",
		},
	}
}

// providedCertTLS loads an operator-supplied cert/key pair and reports its pin
// (so a worker on a private network can pin it instead of installing a CA).
func providedCertTLS(opts tlsOptions) (tlsResult, error) {
	if opts.certFile == "" || opts.keyFile == "" {
		return tlsResult{}, fmt.Errorf("--tls-cert and --tls-key must be set together")
	}
	cert, err := tls.LoadX509KeyPair(opts.certFile, opts.keyFile)
	if err != nil {
		return tlsResult{}, fmt.Errorf("load TLS cert/key: %w", err)
	}
	certPEM, err := os.ReadFile(opts.certFile)
	if err != nil {
		return tlsResult{}, fmt.Errorf("read TLS cert: %w", err)
	}
	pin, err := tlsx.PinForCertPEM(certPEM)
	if err != nil {
		return tlsResult{}, err
	}
	return tlsResult{
		config: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		pin:    pin,
		scheme: "https",
		notes:  []string{"TLS     : enabled (operator-supplied certificate)", "Cert pin: " + pin},
	}, nil
}

// selfSignedTLS loads the cached self-signed cert under stateDir/tls, generating
// and persisting it on first use so the pin stays stable across restarts.
func selfSignedTLS(stateDir string, hosts []string) (tlsResult, error) {
	dir := filepath.Join(stateDir, "tls")
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	certPEM, keyPEM, fresh, err := loadOrCreateSelfSigned(dir, certPath, keyPath, hosts)
	if err != nil {
		return tlsResult{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tlsResult{}, fmt.Errorf("load self-signed keypair: %w", err)
	}
	pin, err := tlsx.PinForCertPEM(certPEM)
	if err != nil {
		return tlsResult{}, err
	}
	origin := "cached"
	if fresh {
		origin = "generated"
	}
	return tlsResult{
		config: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		pin:    pin,
		scheme: "https",
		notes: []string{
			fmt.Sprintf("TLS     : self-signed (%s, %s)", origin, certPath),
			"Cert pin: " + pin,
		},
	}, nil
}

// loadOrCreateSelfSigned returns the cached self-signed PEM pair, generating and
// writing it (key mode 0600) when absent. fresh is true when it was generated.
func loadOrCreateSelfSigned(dir, certPath, keyPath string, hosts []string) (certPEM, keyPEM []byte, fresh bool, err error) {
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		return certPEM, keyPEM, false, nil
	}

	gen, err := tlsx.GenerateSelfSigned(hosts)
	if err != nil {
		return nil, nil, false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { // holds key.pem; owner-only
		return nil, nil, false, fmt.Errorf("create tls dir: %w", err)
	}
	if err := os.WriteFile(certPath, gen.CertPEM, 0o644); err != nil { //nolint:gosec // a cert is public
		return nil, nil, false, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, gen.KeyPEM, 0o600); err != nil {
		return nil, nil, false, fmt.Errorf("write key: %w", err)
	}
	return gen.CertPEM, gen.KeyPEM, true, nil
}
