package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/wire"
)

func newWorkersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workers",
		Short: "Manage workers connected to an Atlas server",
	}
	cmd.AddCommand(newWorkersListCmd())
	cmd.AddCommand(newWorkersRemoveCmd())
	return cmd
}

func newWorkersListCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workers connected to the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if serverURL == "" {
				serverURL = os.Getenv("ATLAS_SERVER_URL")
			}
			if serverURL == "" {
				return fmt.Errorf("--server is required (or set ATLAS_SERVER_URL)")
			}
			return runWorkersList(cmd, serverURL)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "server HTTP URL, e.g. http://server:9090; also ATLAS_SERVER_URL")
	return cmd
}

func newWorkersRemoveCmd() *cobra.Command {
	var serverURL string
	cmd := &cobra.Command{
		Use:   "remove <worker-id>",
		Short: "Gracefully drain and disconnect a worker",
		Long: "remove begins graceful shutdown of a worker: the server stops routing new\n" +
			"requests to it, its in-flight requests finish, then it disconnects.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if serverURL == "" {
				serverURL = os.Getenv("ATLAS_SERVER_URL")
			}
			if serverURL == "" {
				return fmt.Errorf("--server is required (or set ATLAS_SERVER_URL)")
			}
			return runWorkersRemove(cmd, serverURL, args[0])
		},
	}
	cmd.Flags().StringVar(&serverURL, "server", "", "server HTTP URL, e.g. http://server:9090; also ATLAS_SERVER_URL")
	return cmd
}

func runWorkersRemove(cmd *cobra.Command, serverURL, id string) error {
	url := serverURL + "/admin/workers/" + id + "/drain"
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted:
		cmd.Printf("Worker %s draining; it will disconnect once in-flight requests finish.\n", id)
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("no worker %q connected", id)
	default:
		return fmt.Errorf("server returned %s", resp.Status)
	}
}

type workersListResponse struct {
	Workers []struct {
		ID          string        `json:"ID"`
		Name        string        `json:"Name"`
		Hardware    wire.Hardware `json:"Hardware"`
		Version     string        `json:"Version"`
		ConnectedAt time.Time     `json:"ConnectedAt"`
		LastSeen    time.Time     `json:"LastSeen"`
		Draining    bool          `json:"Draining"`
	} `json:"workers"`
}

func runWorkersList(cmd *cobra.Command, serverURL string) error {
	resp, err := http.Get(serverURL + "/admin/workers") //nolint:noctx
	if err != nil {
		return fmt.Errorf("reach server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}

	var body workersListResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if len(body.Workers) == 0 {
		cmd.Println("No workers connected.")
		return nil
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKER ID\tNAME\tPLATFORM\tRAM\tVERSION\tCONNECTED\tSTATUS")
	for _, w := range body.Workers {
		status := "ready"
		if w.Draining {
			status = "draining"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			w.ID,
			w.Name,
			w.Hardware.Platform,
			formatBytes(w.Hardware.RAMBytes),
			w.Version,
			formatAgo(w.ConnectedAt),
			status,
		)
	}
	return tw.Flush()
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fTB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func formatAgo(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
