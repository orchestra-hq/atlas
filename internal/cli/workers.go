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

type workersListResponse struct {
	Workers []struct {
		ID          string        `json:"ID"`
		Name        string        `json:"Name"`
		Hardware    wire.Hardware `json:"Hardware"`
		Version     string        `json:"Version"`
		ConnectedAt time.Time     `json:"ConnectedAt"`
		LastSeen    time.Time     `json:"LastSeen"`
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
	_, _ = fmt.Fprintln(tw, "WORKER ID\tNAME\tPLATFORM\tRAM\tVERSION\tCONNECTED")
	for _, w := range body.Workers {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			w.ID,
			w.Name,
			w.Hardware.Platform,
			formatBytes(w.Hardware.RAMBytes),
			w.Version,
			formatAgo(w.ConnectedAt),
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
