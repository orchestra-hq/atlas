package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
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

func newDeployCmd() *cobra.Command {
	var serverURL string
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
			url, err := resolveServerURL(serverURL)
			if err != nil {
				return err
			}
			return runDeploy(cmd, url, args[0], replicas, worker)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "server HTTP URL, e.g. http://server:9090; also ATLAS_SERVER_URL")
	cmd.Flags().IntVar(&replicas, "replicas", 1, "number of replicas to run across the fleet")
	cmd.Flags().StringVar(&worker, "worker", "", "pin a replica to a specific worker id (else the scheduler best-fits)")
	return cmd
}

func newScaleCmd() *cobra.Command {
	var serverURL string
	var replicas int
	cmd := &cobra.Command{
		Use:   "scale <model> --replicas N",
		Short: "Change a deployment's replica count",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("replicas") {
				return fmt.Errorf("--replicas is required")
			}
			url, err := resolveServerURL(serverURL)
			if err != nil {
				return err
			}
			return runScale(cmd, url, args[0], replicas)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "server HTTP URL, e.g. http://server:9090; also ATLAS_SERVER_URL")
	cmd.Flags().IntVar(&replicas, "replicas", 0, "desired replica count")
	return cmd
}

func newStopCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "stop <model>",
		Short: "Stop a deployment and unload it from the fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, err := resolveServerURL(serverURL)
			if err != nil {
				return err
			}
			return runStop(cmd, url, args[0])
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "server HTTP URL, e.g. http://server:9090; also ATLAS_SERVER_URL")
	return cmd
}

func newDeploymentsCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "deployments",
		Short: "List model deployments and their placement state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := resolveServerURL(serverURL)
			if err != nil {
				return err
			}
			return runDeployments(cmd, url)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "server HTTP URL, e.g. http://server:9090; also ATLAS_SERVER_URL")
	return cmd
}

// postDeployment is the shared body for deploy and scale (POST /admin/deployments).
func postDeployment(cmd *cobra.Command, serverURL, model string, replicas int, worker string) error {
	body, _ := json.Marshal(map[string]any{"model": model, "replicas": replicas, "worker": worker})
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, serverURL+"/admin/deployments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("server returned %s: %s", resp.Status, readError(resp))
	}
	return nil
}

func runDeploy(cmd *cobra.Command, serverURL, model string, replicas int, worker string) error {
	if err := postDeployment(cmd, serverURL, model, replicas, worker); err != nil {
		return err
	}
	cmd.Printf("Deploying %q (%d replica(s)); it becomes routable once a worker reports ready.\n", model, replicas)
	return nil
}

func runScale(cmd *cobra.Command, serverURL, model string, replicas int) error {
	if err := postDeployment(cmd, serverURL, model, replicas, ""); err != nil {
		return err
	}
	cmd.Printf("Scaled %q to %d replica(s).\n", model, replicas)
	return nil
}

func runStop(cmd *cobra.Command, serverURL, model string) error {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodDelete, serverURL+"/admin/deployments/"+model, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
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

func runDeployments(cmd *cobra.Command, serverURL string) error {
	resp, err := http.Get(serverURL + "/admin/deployments") //nolint:noctx
	if err != nil {
		return fmt.Errorf("reach server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
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
