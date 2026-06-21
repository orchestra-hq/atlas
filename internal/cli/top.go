package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/server"
)

const (
	defaultTopInterval = 2 * time.Second
	// clearScreen moves the cursor home and clears the terminal, so each refresh
	// redraws in place rather than scrolling. ANSI is enough for an SSH session;
	// no TUI dependency.
	clearScreen = "\033[H\033[2J"
)

// newTopCmd is the `atlas top` command: an auto-refreshing live view of the fleet
// (M2 phase 1b). It polls the same /admin/status snapshot `atlas status` reads and
// redraws on an interval, adding per-interval rates (requests/sec, tokens/sec,
// errors/sec) computed from successive snapshots. Run it over SSH on the gateway
// box; Ctrl-C exits.
func newTopCmd() *cobra.Command {
	var serverURL, apiKey, tlsPin string
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Live auto-refreshing view of the fleet",
		Long: "top is a live view of the control plane — connected workers, deployments,\n" +
			"and request/token rates — refreshed on an interval. It is the watch-the-fleet\n" +
			"companion to the one-shot `atlas status`. Run it over SSH on the gateway box;\n" +
			"press Ctrl-C to exit.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < time.Second {
				return fmt.Errorf("--interval must be at least 1s")
			}
			client, err := newAdminClient(serverURL, apiKey, tlsPin)
			if err != nil {
				return err
			}
			return runTop(cmd, client, interval)
		},
	}
	adminFlags(cmd, &serverURL, &apiKey, &tlsPin)
	cmd.Flags().DurationVar(&interval, "interval", defaultTopInterval, "refresh interval (minimum 1s)")
	return cmd
}

// topSample is one poll: the snapshot and when it was taken, so the next refresh
// can compute per-second rates from the delta.
type topSample struct {
	status server.FleetStatus
	at     time.Time
}

func runTop(cmd *cobra.Command, client *adminClient, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	out := cmd.OutOrStdout()

	var prev *topSample
	firstFetch := true
	for {
		status, err := fetchFleetStatus(cmd.Context(), client)
		switch {
		case err != nil && firstFetch:
			// Fail fast on a misconfigured target (bad URL, auth, or pin) rather than
			// spinning on a redraw loop that can never succeed.
			return err
		case err != nil:
			// A transient poll failure on an established session: keep the view up and
			// note it, rather than tearing down a live monitor.
			_, _ = fmt.Fprintf(out, "%slast poll failed: %v (retrying in %s)\n", clearScreen, err, interval)
		default:
			cur := &topSample{status: status, at: time.Now()}
			_, _ = fmt.Fprint(out, clearScreen)
			renderTop(out, cur, prev)
			prev = cur
			firstFetch = false
		}

		select {
		case <-cmd.Context().Done():
			return nil
		case <-ticker.C:
		}
	}
}

// renderTop draws one frame: a header with per-interval rates (when a previous
// sample exists), then the shared worker and deployment tables.
func renderTop(out io.Writer, cur, prev *topSample) {
	m := cur.status.Metrics
	_, _ = fmt.Fprintf(out, "atlas top — %s   %d workers, %d deployments\n\n",
		cur.at.Format("15:04:05"), len(cur.status.Workers), len(cur.status.Deployments))

	reqRate, errRate, inRate, outRate := "—", "—", "—", "—"
	if prev != nil {
		if secs := cur.at.Sub(prev.at).Seconds(); secs > 0 {
			p := prev.status.Metrics
			reqRate = fmt.Sprintf("%.1f/s", float64(m.Requests-p.Requests)/secs)
			errRate = fmt.Sprintf("%.1f/s", float64(m.Errors-p.Errors)/secs)
			inRate = fmt.Sprintf("%.0f/s", float64(m.InputTokens-p.InputTokens)/secs)
			outRate = fmt.Sprintf("%.0f/s", float64(m.OutputTokens-p.OutputTokens)/secs)
		}
	}
	_, _ = fmt.Fprintf(out, "REQUESTS  %d total (%s)   %d errors (%s)   %d in flight\n",
		m.Requests, reqRate, m.Errors, errRate, m.InFlight)
	_, _ = fmt.Fprintf(out, "TOKENS    %d in (%s)   %d out (%s)\n", m.InputTokens, inRate, m.OutputTokens, outRate)
	_, _ = fmt.Fprintf(out, "QUEUE     %d queued   %d shed\n", m.QueueDepth, m.Shed)

	_, _ = fmt.Fprintln(out)
	renderWorkersTable(out, cur.status.Workers)
	_, _ = fmt.Fprintln(out)
	renderDeploymentsTable(out, cur.status.Deployments)
}
