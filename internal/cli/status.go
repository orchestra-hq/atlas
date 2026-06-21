package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/server"
)

// newStatusCmd is the `atlas status` command: a one-shot snapshot of the fleet —
// connected workers, model deployments, and headline request/token metrics — read
// over the admin API (M2 phase 1, G15). It is the terminal stand-in for the
// deferred web console; run it on (or pointed at) the gateway box. The live
// auto-refreshing view, `atlas top`, builds on the same data in phase 1b.
func newStatusCmd() *cobra.Command {
	var serverURL, apiKey, tlsPin string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a one-shot snapshot of the fleet",
		Long: "status prints a point-in-time view of the control plane — connected\n" +
			"workers, model deployments, and headline request/token metrics — read over\n" +
			"the admin API. Run it on (or pointed at) the gateway box to see what the\n" +
			"fleet is doing right now.\n\n" +
			"--json emits the same data as a single JSON object, for scripting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAdminClient(serverURL, apiKey, tlsPin)
			if err != nil {
				return err
			}
			return runStatus(cmd, client, asJSON)
		},
	}
	adminFlags(cmd, &serverURL, &apiKey, &tlsPin)
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the snapshot as a JSON object")
	return cmd
}

// fetchFleetStatus GETs and decodes the /admin/status snapshot. Shared by
// `atlas status` (one-shot) and `atlas top` (polled).
func fetchFleetStatus(ctx context.Context, client *adminClient) (server.FleetStatus, error) {
	resp, err := client.do(ctx, http.MethodGet, "/admin/status", nil)
	if err != nil {
		return server.FleetStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := adminStatusError(resp); err != nil {
		return server.FleetStatus{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return server.FleetStatus{}, fmt.Errorf("server returned %s", resp.Status)
	}
	var status server.FleetStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return server.FleetStatus{}, fmt.Errorf("decode response: %w", err)
	}
	return status, nil
}

func runStatus(cmd *cobra.Command, client *adminClient, asJSON bool) error {
	status, err := fetchFleetStatus(cmd.Context(), client)
	if err != nil {
		return err
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	renderStatus(cmd, status)
	return nil
}

func renderStatus(cmd *cobra.Command, status server.FleetStatus) {
	out := cmd.OutOrStdout()
	m := status.Metrics
	_, _ = fmt.Fprintf(out, "REQUESTS  %d total, %d errors, %d in flight\n", m.Requests, m.Errors, m.InFlight)
	_, _ = fmt.Fprintf(out, "TOKENS    %d input, %d output\n", m.InputTokens, m.OutputTokens)
	_, _ = fmt.Fprintln(out)
	renderWorkersTable(out, status.Workers)
	_, _ = fmt.Fprintln(out)
	renderDeploymentsTable(out, status.Deployments)
}

// renderWorkersTable writes the connected-worker table (shared by status and top).
func renderWorkersTable(out io.Writer, workers []server.WorkerInfo) {
	_, _ = fmt.Fprintf(out, "WORKERS (%d)\n", len(workers))
	if len(workers) == 0 {
		_, _ = fmt.Fprintln(out, "  none connected")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKER ID\tNAME\tPLATFORM\tRAM\tCONNECTED\tSTATUS\tMODELS")
	for _, w := range workers {
		s := "ready"
		if w.Draining {
			s = "draining"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			w.ID, w.Name, w.Hardware.Platform, formatBytes(w.Hardware.RAMBytes),
			formatAgo(w.ConnectedAt), s, strings.Join(w.Models, ","))
	}
	_ = tw.Flush()
}

// renderDeploymentsTable writes the deployment table (shared by status and top).
func renderDeploymentsTable(out io.Writer, deps []server.DeploymentInfo) {
	_, _ = fmt.Fprintf(out, "DEPLOYMENTS (%d)\n", len(deps))
	if len(deps) == 0 {
		_, _ = fmt.Fprintln(out, "  none")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tREPLICAS\tREADY\tPENDING")
	for _, d := range deps {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", d.Model, d.Replicas, d.Ready, d.Pending)
	}
	_ = tw.Flush()
}
