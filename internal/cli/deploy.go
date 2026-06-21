package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/tlsx"
)

// serverFlag adds the --server flag shared by the deployment commands and
// resolves it (falling back to ATLAS_SERVER_URL), erroring if unset.
func resolveServerURL(serverURL string) (string, error) {
	if serverURL == "" {
		serverURL = os.Getenv("ATLAS_SERVER_URL")
	}
	if serverURL == "" {
		return "", fmt.Errorf("--server is required (or set ATLAS_SERVER_URL)")
	}
	return serverURL, nil
}

// resolveAdminKey returns the admin API key from the flag, falling back to
// ATLAS_API_KEY. The /admin/* surface requires an admin-scoped key (ADR-0008);
// an empty key is left as-is so the request returns a clear 401.
func resolveAdminKey(apiKey string) string {
	if apiKey == "" {
		apiKey = os.Getenv("ATLAS_API_KEY")
	}
	return apiKey
}

// setAdminAuth attaches the admin API key to a control-plane request.
func setAdminAuth(req *http.Request, apiKey string) {
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
}

// adminStatusError maps an admin auth rejection to a clear, actionable error,
// or nil when the status is not an auth failure.
func adminStatusError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("unauthorized: an admin API key is required (pass --api-key or set ATLAS_API_KEY)")
	case http.StatusForbidden:
		return fmt.Errorf("forbidden: this API key lacks the admin scope (mint one with `atlas keys create --admin`)")
	}
	return nil
}

// adminKeyFlag registers the shared --api-key flag for an admin command.
func adminKeyFlag(cmd *cobra.Command, apiKey *string) {
	cmd.Flags().StringVar(apiKey, "api-key", "", "admin API key to authenticate to the server; also ATLAS_API_KEY")
}

// adminFlags registers the flags every admin command shares: --server, --api-key,
// and --tls-pin. Bind the same three string vars and pass them to newAdminClient.
func adminFlags(cmd *cobra.Command, serverURL, apiKey, tlsPin *string) {
	cmd.Flags().StringVar(serverURL, "server", "", "server HTTP URL, e.g. https://server:9090; also ATLAS_SERVER_URL")
	adminKeyFlag(cmd, apiKey)
	cmd.Flags().StringVar(tlsPin, "tls-pin", "",
		"pin the server's self-signed TLS certificate for an https:// server (sha256:<hex>, printed by 'atlas server --tls-self-signed'); not needed for ACME/public-CA certs; also ATLAS_TLS_PIN")
}

// adminClient is an authenticated HTTP client for the /admin/* control surface.
// It carries the server base URL, the admin API key, and an *http.Client that is
// either the default (system trust) or pinned to a self-signed certificate
// (--tls-pin), so admin commands reach a self-signed-TLS gateway the same way a
// worker does (ADR-0009).
type adminClient struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// newAdminClient resolves the server URL (or ATLAS_SERVER_URL), the admin key (or
// ATLAS_API_KEY), and the optional cert pin (or ATLAS_TLS_PIN) into a ready
// client. A malformed pin, or a pin against a non-https URL, fails here rather
// than at request time.
func newAdminClient(serverURL, apiKey, flagPin string) (*adminClient, error) {
	url, err := resolveServerURL(serverURL)
	if err != nil {
		return nil, err
	}
	hc, err := pinnedHTTPClient(flagPin, url)
	if err != nil {
		return nil, err
	}
	return &adminClient{baseURL: strings.TrimRight(url, "/"), apiKey: resolveAdminKey(apiKey), hc: hc}, nil
}

// pinnedHTTPClient returns the HTTP client admin commands use. With no pin it is
// http.DefaultClient (system trust — the ACME/public-CA or plaintext case). With
// a pin (flag or ATLAS_TLS_PIN) it verifies the server's leaf certificate against
// the pin in place of CA/hostname validation, the admin-side mirror of the
// worker's --tls-pin (ADR-0009). A pin requires an https:// server URL — there is
// no TLS certificate to pin on plain http://, so that combination is a hard error
// rather than a silently unpinned (and unencrypted) connection.
func pinnedHTTPClient(flagPin, serverURL string) (*http.Client, error) {
	pin := flagPin
	if pin == "" {
		pin = os.Getenv("ATLAS_TLS_PIN")
	}
	if pin == "" {
		return http.DefaultClient, nil
	}
	normalized, err := tlsx.NormalizePin(pin)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(serverURL, "https://") {
		return nil, fmt.Errorf("--tls-pin is set but the server URL is not https:// (%s); use an https:// URL or drop the pin", serverURL)
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // not insecure: VerifyConnection pins the exact leaf cert
			VerifyConnection:   tlsx.PinnedVerifier(normalized),
		},
	}}, nil
}

// do issues an authenticated admin request to path (joined to the base URL),
// setting Content-Type when a body is sent. The caller closes the response body.
func (c *adminClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setAdminAuth(req, c.apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach server: %w", err)
	}
	return resp, nil
}

func newDeployCmd() *cobra.Command {
	var serverURL, apiKey, tlsPin string
	var replicas int
	var worker string
	cmd := &cobra.Command{
		Use:   "deploy <model>",
		Short: "Place a model on the fleet via the scheduler",
		Long: "deploy asks the server's scheduler to load a catalog model onto a fitting\n" +
			"worker (by VRAM), or onto a specific worker with --worker. The gateway\n" +
			"routes to it once it reports ready.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAdminClient(serverURL, apiKey, tlsPin)
			if err != nil {
				return err
			}
			return runDeploy(cmd, client, args[0], replicas, worker)
		},
	}
	cmd.Flags().IntVar(&replicas, "replicas", 1, "number of replicas to run across the fleet")
	cmd.Flags().StringVar(&worker, "worker", "", "pin a replica to a specific worker id (else the scheduler best-fits)")
	adminFlags(cmd, &serverURL, &apiKey, &tlsPin)
	return cmd
}

func newScaleCmd() *cobra.Command {
	var serverURL, apiKey, tlsPin string
	var replicas int
	cmd := &cobra.Command{
		Use:   "scale <model> --replicas N",
		Short: "Change a deployment's replica count",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("replicas") {
				return fmt.Errorf("--replicas is required")
			}
			client, err := newAdminClient(serverURL, apiKey, tlsPin)
			if err != nil {
				return err
			}
			return runScale(cmd, client, args[0], replicas)
		},
	}
	cmd.Flags().IntVar(&replicas, "replicas", 0, "desired replica count")
	adminFlags(cmd, &serverURL, &apiKey, &tlsPin)
	return cmd
}

func newStopCmd() *cobra.Command {
	var serverURL, apiKey, tlsPin string
	cmd := &cobra.Command{
		Use:   "stop <model>",
		Short: "Stop a deployment and unload it from the fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAdminClient(serverURL, apiKey, tlsPin)
			if err != nil {
				return err
			}
			return runStop(cmd, client, args[0])
		},
	}
	adminFlags(cmd, &serverURL, &apiKey, &tlsPin)
	return cmd
}

func newDeploymentsCmd() *cobra.Command {
	var serverURL, apiKey, tlsPin string
	cmd := &cobra.Command{
		Use:   "deployments",
		Short: "List model deployments and their placement state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAdminClient(serverURL, apiKey, tlsPin)
			if err != nil {
				return err
			}
			return runDeployments(cmd, client)
		},
	}
	adminFlags(cmd, &serverURL, &apiKey, &tlsPin)
	return cmd
}

// postDeployment is the shared body for deploy and scale (POST /admin/deployments).
func postDeployment(cmd *cobra.Command, client *adminClient, model string, replicas int, worker string) error {
	body, _ := json.Marshal(map[string]any{"model": model, "replicas": replicas, "worker": worker})
	resp, err := client.do(cmd.Context(), http.MethodPost, "/admin/deployments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := adminStatusError(resp); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("server returned %s: %s", resp.Status, readError(resp))
	}
	return nil
}

func runDeploy(cmd *cobra.Command, client *adminClient, model string, replicas int, worker string) error {
	if err := postDeployment(cmd, client, model, replicas, worker); err != nil {
		return err
	}
	cmd.Printf("Deploying %q (%d replica(s)); it becomes routable once a worker reports ready.\n", model, replicas)
	return nil
}

func runScale(cmd *cobra.Command, client *adminClient, model string, replicas int) error {
	if err := postDeployment(cmd, client, model, replicas, ""); err != nil {
		return err
	}
	cmd.Printf("Scaled %q to %d replica(s).\n", model, replicas)
	return nil
}

func runStop(cmd *cobra.Command, client *adminClient, model string) error {
	resp, err := client.do(cmd.Context(), http.MethodDelete, "/admin/deployments/"+model, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := adminStatusError(resp); err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusNoContent:
		cmd.Printf("Stopped %q; unloading from the fleet.\n", model)
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("no deployment for %q", model)
	default:
		return fmt.Errorf("server returned %s", resp.Status)
	}
}

type deploymentsResponse struct {
	Deployments []struct {
		Model    string `json:"model"`
		Replicas int    `json:"replicas"`
		Ready    int    `json:"ready"`
		Pending  int    `json:"pending"`
	} `json:"deployments"`
}

func runDeployments(cmd *cobra.Command, client *adminClient) error {
	resp, err := client.do(cmd.Context(), http.MethodGet, "/admin/deployments", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := adminStatusError(resp); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	var body deploymentsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(body.Deployments) == 0 {
		cmd.Println("No deployments.")
		return nil
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tREPLICAS\tREADY\tPENDING")
	for _, d := range body.Deployments {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", d.Model, d.Replicas, d.Ready, d.Pending)
	}
	return tw.Flush()
}

// readError returns a short snippet of an error response body for messages.
func readError(resp *http.Response) string {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(http.MaxBytesReader(nil, resp.Body, 1<<16))
	return buf.String()
}
