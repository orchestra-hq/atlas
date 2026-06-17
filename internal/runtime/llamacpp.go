package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultReleaseBaseURL is where pinned llama.cpp release assets are fetched
// from. Overridable (tests, mirrors) via Provisioner.BaseURL.
const DefaultReleaseBaseURL = "https://github.com/ggml-org/llama.cpp/releases/download"

// LlamaCppTag is the pinned llama.cpp release. Upgrades are explicit
// (build-time decision 5): bump the tag and the checksums together, then let
// the conformance matrix gate the change.
const LlamaCppTag = "b9611"

// llamaCppAssets pins the SHA-256 of each supported platform archive for
// LlamaCppTag. Digests are from the GitHub release; verify on upgrade with:
//
//	gh api repos/ggml-org/llama.cpp/releases/tags/<tag> \
//	  --jq '.assets[] | {name, digest}'
var llamaCppAssets = map[string]asset{
	"darwin/arm64": {"llama-" + LlamaCppTag + "-bin-macos-arm64.tar.gz", "544282530e3833b113b739263af822a889c33ded15163d01f7a77bb41622eef8"},
	"darwin/amd64": {"llama-" + LlamaCppTag + "-bin-macos-x64.tar.gz", "87722317da7f170fcd4122d9b6ea94d7b6d27968d29d3cccc490d75c1ad61280"},
	"linux/arm64":  {"llama-" + LlamaCppTag + "-bin-ubuntu-arm64.tar.gz", "ea16ee97d2dd29ddf826e7ff3b04ae9fc3df5bca3db4216cd8610883b356f90d"},
	"linux/amd64":  {"llama-" + LlamaCppTag + "-bin-ubuntu-x64.tar.gz", "8ada3fdae5933813c43551052bdbd2ff5fef10a5ab205827d5d208dbc3725cb7"},
}

// EnsureLlamaServer makes the pinned llama-server available for the given
// platform (GOOS/GOARCH) and returns its absolute path. It is idempotent: if
// the binary is already unpacked, it returns immediately without downloading.
func (p *Provisioner) EnsureLlamaServer(ctx context.Context, goos, goarch string) (string, error) {
	a, ok := llamaCppAssets[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("runtime: no pinned llama.cpp build for %s/%s", goos, goarch)
	}
	return p.ensureAsset(ctx, a)
}

func (p *Provisioner) ensureAsset(ctx context.Context, a asset) (string, error) {
	dest := filepath.Join(p.Dir, "llamacpp", LlamaCppTag)
	binPath := filepath.Join(dest, "llama-server")
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	base := p.BaseURL
	if base == "" {
		base = DefaultReleaseBaseURL
	}
	if err := p.installRelease(ctx, base, LlamaCppTag, a, dest, "llamacpp-*.tar.gz"); err != nil {
		return "", err
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("runtime: llama-server missing after extracting %s", a.name)
	}
	return binPath, nil
}
