package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/db"
	"github.com/orchestra-hq/atlas/internal/server"
)

// auditRecorder bridges the gateway's server.AuditRecorder to the control-plane
// SQLite store (M3 phase 3). A write failure is logged and swallowed: the mutation
// it describes has already happened, so failing to record it must not fail the
// request.
type auditRecorder struct {
	db  *db.DB
	log *slog.Logger
}

func (a auditRecorder) RecordAudit(ctx context.Context, e server.AuditEvent) error {
	err := a.db.RecordAudit(ctx, db.AuditEntry{
		Actor:  e.Actor,
		Action: e.Action,
		Target: e.Target,
		Result: e.Result,
		Detail: e.Detail,
	})
	if err != nil && a.log != nil {
		a.log.Warn("audit record failed", "action", e.Action, "target", e.Target, "error", err)
	}
	return err
}

// auditListHandler serves GET /admin/audit, the admin read API over the audit log
// (M3 phase 3). It reads the same filters `atlas audit` exposes from the query
// string — actor, action, target, since/until (RFC3339), limit — and returns the
// matching rows newest-first as JSON.
func auditListHandler(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := auditFilterFromQuery(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		entries, err := store.ListAudit(r.Context(), f)
		if err != nil {
			http.Error(w, "could not read audit log", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(auditRowsJSON(entries))
	}
}

// auditFilterFromQuery builds a db.AuditFilter from request query parameters,
// rejecting a malformed since/until/limit with an error the handler turns into a 400.
func auditFilterFromQuery(r *http.Request) (db.AuditFilter, error) {
	q := r.URL.Query()
	f := db.AuditFilter{Actor: q.Get("actor"), Action: q.Get("action"), Target: q.Get("target")}
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return db.AuditFilter{}, fmt.Errorf("invalid since (want RFC3339): %v", err)
		}
		f.Since = t
	}
	if s := q.Get("until"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return db.AuditFilter{}, fmt.Errorf("invalid until (want RFC3339): %v", err)
		}
		f.Until = t
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return db.AuditFilter{}, fmt.Errorf("invalid limit: %v", err)
		}
		f.Limit = n
	}
	return f, nil
}

// auditRowJSON is one audit row in the read API / CLI JSON output.
type auditRowJSON struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
}

func auditRowsJSON(entries []db.AuditEntry) []auditRowJSON {
	rows := make([]auditRowJSON, len(entries))
	for i, e := range entries {
		rows[i] = auditRowJSON{Time: e.Time, Actor: e.Actor, Action: e.Action, Target: e.Target, Result: e.Result, Detail: e.Detail}
	}
	return rows
}

// newAuditCmd is the `atlas audit` command: it lists the control-plane audit log
// (M3 phase 3, G21). Like `atlas keys` and `atlas usage` it reads the control-plane
// SQLite file under --state-dir directly, so it works whether or not a server is
// running and reflects every mutation recorded so far. Filters narrow by actor,
// action, target, and time window.
func newAuditCmd() *cobra.Command {
	var (
		stateDir string
		actor    string
		action   string
		target   string
		since    string
		until    string
		limit    int
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "List the control-plane audit log",
		Long: "audit lists control-plane mutations — deployments set/stopped, workers\n" +
			"drained, API keys created/revoked — each with the actor, action, target,\n" +
			"result, and time. The log is append-only and durable across restarts.\n\n" +
			"Filter with --actor, --action, --target, and --since/--until (RFC3339).\n" +
			"--json emits the rows as a JSON array, for scripting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStateDB(stateDir)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			f := db.AuditFilter{Actor: actor, Action: action, Target: target, Limit: limit}
			if since != "" {
				t, perr := time.Parse(time.RFC3339, since)
				if perr != nil {
					return fmt.Errorf("invalid --since (want RFC3339, e.g. 2026-06-22T00:00:00Z): %w", perr)
				}
				f.Since = t
			}
			if until != "" {
				t, perr := time.Parse(time.RFC3339, until)
				if perr != nil {
					return fmt.Errorf("invalid --until (want RFC3339): %w", perr)
				}
				f.Until = t
			}

			entries, err := store.ListAudit(cmd.Context(), f)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(auditRowsJSON(entries))
			}
			renderAudit(cmd, entries)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "control-plane state directory holding the audit log")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor (an admin key id, or cli/system)")
	cmd.Flags().StringVar(&action, "action", "", "filter by action (e.g. deployment.set, worker.drain, key.create)")
	cmd.Flags().StringVar(&target, "target", "", "filter by target (a model name, worker id, or key id)")
	cmd.Flags().StringVar(&since, "since", "", "only entries at or after this RFC3339 time")
	cmd.Flags().StringVar(&until, "until", "", "only entries before this RFC3339 time")
	cmd.Flags().IntVar(&limit, "limit", 0, "max entries to show (0 = default cap)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit entries as a JSON array")
	return cmd
}

// renderAudit writes the audit entries as a table, newest first.
func renderAudit(cmd *cobra.Command, entries []db.AuditEntry) {
	if len(entries) == 0 {
		cmd.Println("No audit entries.")
		return
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tACTOR\tACTION\tTARGET\tRESULT\tDETAIL")
	for _, e := range entries {
		target := e.Target
		if target == "" {
			target = "—"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, target, e.Result, e.Detail)
	}
	_ = tw.Flush()
}
