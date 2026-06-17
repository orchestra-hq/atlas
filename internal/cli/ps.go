package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/api/anthropic"
)

type psOptions struct {
	addr   string
	apiKey string
}

func newPsCmd() *cobra.Command {
	opts := &psOptions{}
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "Show the models a running Atlas instance is serving",
		Long: "ps probes a running `atlas up` over its HTTP endpoint: liveness\n" +
			"(/healthz), readiness (/readyz), and the served models with their context\n" +
			"windows and aliases (/v1/models). Point it at a non-default endpoint with\n" +
			"--addr; the API key comes from --api-key or $ATLAS_API_KEY.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPs(cmd.Context(), cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", "127.0.0.1:8080", "address of the running Atlas gateway")
	cmd.Flags().StringVar(&opts.apiKey, "api-key", os.Getenv("ATLAS_API_KEY"), "API key for /v1/models (defaults to $ATLAS_API_KEY)")
	return cmd
}

func runPs(ctx context.Context, cmd *cobra.Command, opts *psOptions) error {
	base := "http://" + opts.addr
	client := &http.Client{Timeout: 5 * time.Second}

	// Liveness first: a clear message beats a wall of connection errors when
	// nothing is running.
	if code, err := probe(ctx, client, base+"/healthz"); err != nil {
		return fmt.Errorf("no Atlas instance reachable at %s (is `atlas up` running?): %w", opts.addr, err)
	} else if code != http.StatusOK {
		return fmt.Errorf("atlas at %s is unhealthy (/healthz returned %d)", opts.addr, code)
	}

	ready := "not ready"
	if code, err := probe(ctx, client, base+"/readyz"); err == nil && code == http.StatusOK {
		ready = "ready"
	}
	cmd.Printf("Atlas at %s — %s\n\n", opts.addr, ready)

	list, err := fetchModels(ctx, client, base, opts.apiKey)
	if err != nil {
		return err
	}
	if len(list.Data) == 0 {
		cmd.Println("No models served.")
		return nil
	}

	cmd.Printf("%-32s %-14s %s\n", "MODEL", "CONTEXT", "RESOLVES TO")
	for _, m := range list.Data {
		ctxWindow := "—"
		if m.ContextWindow > 0 {
			ctxWindow = fmt.Sprintf("%d", m.ContextWindow)
		}
		resolves := ""
		if m.DisplayName != "" && m.DisplayName != m.ID {
			resolves = m.DisplayName // an alias points at its canonical model
		}
		cmd.Printf("%-32s %-14s %s\n", m.ID, ctxWindow, resolves)
	}
	return nil
}

// probe issues a GET and returns the status code, or an error if the endpoint
// is unreachable.
func probe(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// fetchModels lists the served models from /v1/models, which requires auth.
func fetchModels(ctx context.Context, client *http.Client, base, apiKey string) (anthropic.ModelList, error) {
	if apiKey == "" {
		return anthropic.ModelList{}, fmt.Errorf("no API key: pass --api-key or set ATLAS_API_KEY to list models")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return anthropic.ModelList{}, err
	}
	req.Header.Set("x-api-key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return anthropic.ModelList{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return anthropic.ModelList{}, fmt.Errorf("unauthorized listing models: check --api-key / $ATLAS_API_KEY")
	}
	if resp.StatusCode != http.StatusOK {
		return anthropic.ModelList{}, fmt.Errorf("listing models: /v1/models returned %d", resp.StatusCode)
	}
	var list anthropic.ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return anthropic.ModelList{}, fmt.Errorf("decode model list: %w", err)
	}
	return list, nil
}
