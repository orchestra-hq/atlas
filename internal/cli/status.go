package cli

import (
	"encoding/json"
	"fmt"
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

func runStatus(cmd *cobra.Command, client *adminClient, asJSON bool) error {
	resp, err := client.do(cmd.Context(), http.MethodGet, "/admin/status", nil)
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

	var status server.FleetStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("decode response: %w", err)
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
	m := status.Metrics
	cmd.Printf("REQUESTS  %d total, %d errors, %d in flight\n", m.Requests, m.Errors, m.InFlight)
	cmd.Printf("TOKENS    %d input, %d output\n", m.InputTokens, m.OutputTokens)

	cmd.Println()
	cmd.Printf("WORKERS (%d)\n", len(status.Workers))
	if len(status.Workers) == 0 {
		cmd.Println("  none connected")
	} else {
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "WORKER ID\tNAME\tPLATFORM\tRAM\tCONNECTED\tSTATUS\tMODELS")
		for _, w := range status.Workers {
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

	cmd.Println()
	cmd.Printf("DEPLOYMENTS (%d)\n", len(status.Deployments))
	if len(status.Deployments) == 0 {
		cmd.Println("  none")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tREPLICAS\tREADY\tPENDING")
	for _, d := range status.Deployments {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", d.Model, d.Replicas, d.Ready, d.Pending)
	}
	_ = tw.Flush()
}
