package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/db"
)

// newUsageCmd is the `atlas usage` command: per-key, per-model, and per-worker
// token totals from the durable usage ledger (phase 6, G13). Like `atlas keys`
// it reads the control-plane SQLite file under --state-dir directly, so it works
// whether or not a server is running and reflects every request recorded so far.
func newUsageCmd() *cobra.Command {
	var (
		stateDir string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show token usage totals by key, model, and worker",
		Long: "usage summarizes the durable usage ledger: how many requests each API\n" +
			"key made and how many input/output tokens each key, model, and worker\n" +
			"accounted for. Totals are cumulative since the ledger was created.\n\n" +
			"--json emits the same data as a single JSON object, for scripting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStateDB(stateDir)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			rep, err := collectUsage(cmd.Context(), store)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			rep.render(cmd)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "control-plane state directory holding the usage ledger")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit totals as a JSON object")
	return cmd
}

// usageRow is one group's totals in the JSON/table output.
type usageRow struct {
	Group        string `json:"group"`
	Requests     int    `json:"requests"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// usageReport is the full `atlas usage` result: the ledger summarized three ways.
type usageReport struct {
	ByKey    []usageRow `json:"by_key"`
	ByModel  []usageRow `json:"by_model"`
	ByWorker []usageRow `json:"by_worker"`
}

func collectUsage(ctx context.Context, store *db.DB) (usageReport, error) {
	byKey, err := store.UsageByKey(ctx)
	if err != nil {
		return usageReport{}, err
	}
	byModel, err := store.UsageByModel(ctx)
	if err != nil {
		return usageReport{}, err
	}
	byWorker, err := store.UsageByWorker(ctx)
	if err != nil {
		return usageReport{}, err
	}
	return usageReport{
		ByKey:    toUsageRows(byKey),
		ByModel:  toUsageRows(byModel),
		ByWorker: toUsageRows(byWorker),
	}, nil
}

func toUsageRows(totals []db.UsageTotal) []usageRow {
	rows := make([]usageRow, len(totals))
	for i, t := range totals {
		rows[i] = usageRow{Group: t.Group, Requests: t.Requests, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens}
	}
	return rows
}

func (r usageReport) render(cmd *cobra.Command) {
	if len(r.ByModel) == 0 {
		cmd.Println("No usage recorded yet.")
		return
	}
	renderUsageTable(cmd, "BY MODEL", "MODEL", r.ByModel)
	cmd.Println()
	renderUsageTable(cmd, "BY KEY", "KEY ID", r.ByKey)
	cmd.Println()
	renderUsageTable(cmd, "BY WORKER", "WORKER", r.ByWorker)
}

func renderUsageTable(cmd *cobra.Command, title, groupHeader string, rows []usageRow) {
	cmd.Println(title)
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "%s\tREQUESTS\tINPUT\tOUTPUT\tTOTAL\n", groupHeader)
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n",
			row.Group, row.Requests, row.InputTokens, row.OutputTokens, row.InputTokens+row.OutputTokens)
	}
	_ = tw.Flush()
}
