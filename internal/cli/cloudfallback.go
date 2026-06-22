package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/orchestra-hq/atlas/internal/server"
)

// defaultCloudBaseURL is the upstream host for each provider when a --cloud-fallback
// spec omits one.
var defaultCloudBaseURL = map[string]string{
	server.CloudProviderAnthropic: "https://api.anthropic.com",
	server.CloudProviderOpenAI:    "https://api.openai.com",
}

// parseCloudFallback turns repeated --cloud-fallback specs into per-model upstream
// targets (M3 phase 4, ADR-0013). Each spec is
//
//	localModel:provider:upstreamModel:keyEnv[:baseURL]
//
// where keyEnv names the environment variable holding the upstream API key — the key
// is never passed on the command line, so it does not leak into the process list. An
// omitted baseURL defaults per provider. An empty list (the default) means fallback
// is off and no request ever leaves the operator's infrastructure.
func parseCloudFallback(specs []string) (map[string]server.CloudTarget, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	targets := make(map[string]server.CloudTarget, len(specs))
	for _, spec := range specs {
		// SplitN with 5 so a baseURL containing ":" (the scheme) survives intact.
		parts := strings.SplitN(spec, ":", 5)
		if len(parts) < 4 {
			return nil, fmt.Errorf("cloud-fallback %q: want localModel:provider:upstreamModel:keyEnv[:baseURL]", spec)
		}
		localModel, provider, upstreamModel, keyEnv := parts[0], parts[1], parts[2], parts[3]
		if localModel == "" || upstreamModel == "" {
			return nil, fmt.Errorf("cloud-fallback %q: localModel and upstreamModel are required", spec)
		}
		baseURL, known := defaultCloudBaseURL[provider]
		if !known {
			return nil, fmt.Errorf("cloud-fallback %q: provider must be %s or %s", spec, server.CloudProviderAnthropic, server.CloudProviderOpenAI)
		}
		if len(parts) == 5 && parts[4] != "" {
			baseURL = parts[4]
		}
		key := os.Getenv(keyEnv)
		if key == "" {
			return nil, fmt.Errorf("cloud-fallback %q: environment variable %s (the upstream API key) is empty", spec, keyEnv)
		}
		if _, dup := targets[localModel]; dup {
			return nil, fmt.Errorf("cloud-fallback: model %q has more than one target", localModel)
		}
		targets[localModel] = server.CloudTarget{
			Provider: provider,
			BaseURL:  baseURL,
			Model:    upstreamModel,
			APIKey:   key,
		}
	}
	return targets, nil
}
