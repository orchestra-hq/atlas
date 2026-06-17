package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/orchestra-hq/atlas/catalog"
	"github.com/orchestra-hq/atlas/internal/store"
)

type pullOptions struct {
	stateDir string
}

func newPullCmd() *cobra.Command {
	opts := &pullOptions{}
	cmd := &cobra.Command{
		Use:   "pull [model...]",
		Short: "Download catalog models into the local store",
		Long: "pull fetches a starter-catalog model's weights into the content-addressable\n" +
			"store under the state dir, verifying them against the catalog's pinned\n" +
			"sha256. Run with no arguments to list the catalog. `atlas up` pulls on\n" +
			"demand too; pull is the way to warm the store ahead of time.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPull(cmd.Context(), cmd, opts, args)
		},
	}
	cmd.Flags().StringVar(&opts.stateDir, "state-dir", defaultStateDir(), "directory for runtimes, weights, and logs")
	return cmd
}

func runPull(ctx context.Context, cmd *cobra.Command, opts *pullOptions, names []string) error {
	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return listCatalog(cmd, cat)
	}

	st := store.New(filepath.Join(opts.stateDir, "store"))
	for _, name := range names {
		entry, ok := cat.Lookup(name)
		if !ok {
			return fmt.Errorf("unknown model %q — run `atlas pull` to list the catalog", name)
		}
		switch entry.Source.Type {
		case "gguf":
			if st.Has(entry.Name) {
				cmd.Printf("%s already in the store.\n", entry.Name)
				continue
			}
			if err := pullEntry(ctx, cmd, st, entry); err != nil {
				return err
			}
		case "hf":
			cmd.Printf("%s is a %s model fetched by the engine on first `atlas up` — nothing to pre-pull.\n", entry.Name, entry.Engine)
		}
	}
	return nil
}

func listCatalog(cmd *cobra.Command, cat *catalog.Catalog) error {
	cmd.Println(catalog.Header())
	for _, e := range cat.All() {
		cmd.Println(e.Summary())
	}
	cmd.Println("\nPull one with: atlas pull <name>")
	return nil
}
