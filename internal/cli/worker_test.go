package cli

import (
	"strings"
	"testing"

	"github.com/orchestra-hq/atlas/internal/tlsx"
)

func TestResolveWorkerPin(t *testing.T) {
	// A normalizable pin (lower-cased, sha256: prefixed) is what reaches the dialer.
	validPin := "sha256:" + strings.Repeat("ab", 32)

	t.Run("no pin is allowed on any scheme", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_PIN", "")
		got, err := resolveWorkerPin("", "ws://server:9090/workers/connect")
		if err != nil || got != "" {
			t.Fatalf("resolveWorkerPin(\"\", ws) = (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("pin on wss is normalized and returned", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_PIN", "")
		want, _ := tlsx.NormalizePin(validPin)
		got, err := resolveWorkerPin(validPin, "wss://server:9090/workers/connect")
		if err != nil {
			t.Fatalf("resolveWorkerPin on wss errored: %v", err)
		}
		if got != want {
			t.Errorf("resolveWorkerPin = %q, want normalized %q", got, want)
		}
	})

	t.Run("pin on non-wss is a hard error, not a silent downgrade", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_PIN", "")
		if _, err := resolveWorkerPin(validPin, "ws://server:9090/workers/connect"); err == nil {
			t.Fatal("resolveWorkerPin with a pin on a ws:// URL returned nil; want an error so the worker does not dial plaintext with the pin silently dropped")
		}
	})

	t.Run("malformed pin fails fast", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_PIN", "")
		if _, err := resolveWorkerPin("not-a-pin", "wss://server:9090/workers/connect"); err == nil {
			t.Fatal("resolveWorkerPin with a malformed pin returned nil; want an error")
		}
	})

	t.Run("pin from ATLAS_TLS_PIN is honored", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_PIN", validPin)
		got, err := resolveWorkerPin("", "wss://server:9090/workers/connect")
		if err != nil || got == "" {
			t.Fatalf("resolveWorkerPin from env = (%q, %v), want a normalized pin", got, err)
		}
	})
}
