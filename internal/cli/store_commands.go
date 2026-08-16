package cli

import (
	"fmt"
	"strings"

	"github.com/codesweep-ai/sandbox/internal/store"
	"github.com/spf13/cobra"
)

func (a *App) storeManager() store.Manager {
	return store.Manager{Runner: a.Runner, Image: a.Image}
}

func newStoreCmds(app *App) []*cobra.Command {
	createStore := &cobra.Command{
		Use:   "create-store <name>",
		Short: "Create an empty shared image store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.storeManager().Create(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created empty shared store %q — seed it with: cs-sandbox seed-store %s <image>...\n", args[0], args[0])
			return nil
		},
	}

	var fromHost bool
	seedStore := &cobra.Command{
		Use:               "seed-store [--from-host] <name> <image>...",
		Short:             "Populate a shared store by pulling (or copying host) images",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: app.completeStore,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, images := args[0], args[1:]
			if err := app.storeManager().Seed(cmd.Context(), name, images, fromHost); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "seeded shared store %q — use it with: cs-sandbox create <name> --image-store %s\n", name, name)
			return nil
		},
	}
	seedStore.Flags().BoolVar(&fromHost, "from-host", false, "copy+re-own images already in the host store instead of pulling")

	stores := &cobra.Command{
		Use:   "stores",
		Short: "List shared image stores and their images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m := app.storeManager()
			names := m.List(cmd.Context())
			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no shared stores (create one with: cs-sandbox create-store <name>)")
				return nil
			}
			for _, name := range names {
				fmt.Fprintf(cmd.OutOrStdout(), "== %s ==\n", name)
				imgs, err := m.Images(cmd.Context(), name)
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  (unreadable — re-create with: cs-sandbox rm-store %s && cs-sandbox seed-store ...)\n", name)
					continue
				}
				if imgs == "" {
					fmt.Fprintln(cmd.OutOrStdout(), "  (no images)")
					continue
				}
				for _, line := range splitLines(imgs) {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", line)
				}
			}
			return nil
		},
	}

	var force bool
	rmStore := &cobra.Command{
		Use:               "rm-store [-f] <name>",
		Short:             "Delete a shared image store",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeStore,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := app.storeManager().Remove(cmd.Context(), args[0], force); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed shared store %q\n", args[0])
			return nil
		},
	}
	rmStore.Flags().BoolVarP(&force, "force", "f", false, "remove even if in use")

	return []*cobra.Command{createStore, seedStore, stores, rmStore}
}

func splitLines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
