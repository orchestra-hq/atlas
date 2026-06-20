package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/internal/db"
)

// newKeysCmd is the `atlas keys` command group: client API key management
// (ADR-0008). The subcommands operate directly on the control-plane SQLite file
// under --state-dir (the same host the server runs on), not over HTTP — so they
// work whether or not a server is currently running, and a revoke takes effect
// on the next request the server authenticates.
func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage client API keys for the Atlas gateway",
	}
	cmd.AddCommand(newKeysCreateCmd())
	cmd.AddCommand(newKeysListCmd())
	cmd.AddCommand(newKeysRevokeCmd())
	return cmd
}

func newKeysCreateCmd() *cobra.Command {
	var (
		stateDir string
		allow    []string
		admin    bool
		quiet    bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a new API key and print it once",
		Long: "create mints a new API key, prints the secret once (only its hash is\n" +
			"stored — save it now), and records its model allowlist and scope.\n\n" +
			"With no --allow flags the key may use every model; repeat --allow to\n" +
			"restrict it to specific models. --admin grants access to the /admin/*\n" +
			"control surface (deploy/scale/stop, worker management).\n\n" +
			"--quiet prints only the secret on a single line, for scripting\n" +
			"(e.g. ATLAS_API_KEY=$(atlas keys create --quiet)).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStateDB(stateDir)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			secret, k, err := store.CreateKey(cmd.Context(), allow, admin)
			if err != nil {
				return err
			}
			if quiet {
				// Print only the secret, on stdout, for `KEY=$(atlas keys create
				// --quiet)`. cobra's cmd.Print* default to stderr, so write to
				// OutOrStdout explicitly — otherwise command substitution captures
				// nothing.
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), secret)
				return nil
			}
			cmd.Printf("Created key %s\n", k.ID)
			cmd.Printf("  Secret    : %s\n", secret)
			cmd.Printf("  Scope     : %s\n", scopeLabel(k.Admin))
			cmd.Printf("  Models    : %s\n", allowlistLabel(k.Allowlist))
			cmd.Println("\nSave the secret now — it is not shown again.")
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "control-plane state directory holding the key store")
	cmd.Flags().StringArrayVar(&allow, "allow", nil, "restrict the key to this model; repeat for several (default: all models)")
	cmd.Flags().BoolVar(&admin, "admin", false, "grant the admin scope (access to the /admin/* control surface)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print only the secret, for scripting")
	return cmd
}

func newKeysListCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API keys (secrets are never shown)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStateDB(stateDir)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			keys, err := store.ListKeys(cmd.Context())
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				cmd.Println("No API keys. Create one with `atlas keys create`.")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "KEY ID\tPREFIX\tSCOPE\tMODELS\tCREATED\tSTATUS")
			for _, k := range keys {
				_, _ = fmt.Fprintf(tw, "%s\t%s…\t%s\t%s\t%s\t%s\n",
					k.ID, k.Prefix, scopeLabel(k.Admin), allowlistLabel(k.Allowlist),
					formatAgo(k.CreatedAt), keyStatus(k))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "control-plane state directory holding the key store")
	return cmd
}

func newKeysRevokeCmd() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an API key by id",
		Long: "revoke invalidates a key immediately: the next request presenting it is\n" +
			"rejected with 401. Use the key id from `atlas keys list`, not the secret.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStateDB(stateDir)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			switch err := store.RevokeKey(cmd.Context(), args[0]); {
			case err == nil:
				cmd.Printf("Revoked key %s.\n", args[0])
				return nil
			case errors.Is(err, db.ErrKeyNotFound):
				return fmt.Errorf("no key with id %q", args[0])
			default:
				return err
			}
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "control-plane state directory holding the key store")
	return cmd
}

func scopeLabel(admin bool) string {
	if admin {
		return "admin"
	}
	return "client"
}

func allowlistLabel(allow []string) string {
	if len(allow) == 0 {
		return "all"
	}
	return strings.Join(allow, ",")
}

func keyStatus(k db.Key) string {
	if k.Revoked() {
		return "revoked"
	}
	return "active"
}
