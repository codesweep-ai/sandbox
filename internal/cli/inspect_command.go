package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/state"
	"github.com/spf13/cobra"
)

// inspectItem is the stable shape of `inspect --json`: everything about one
// sandbox that another tool has a reason to read.
//
// It exists because the alternative is a consumer deriving these values for
// itself. A branch name or an instance path is spelled by a rule that lives in
// this repository, and a caller that reimplements the rule agrees with it only
// until the rule changes — then it computes a plausible answer that is wrong,
// which is a far worse failure than not being able to ask. `ls --json` already
// settled that argument for the inventory; this is the same answer for one
// sandbox's detail.
//
// A deliberate shape, not the on-disk record: state.Instance is free to change
// without breaking anyone reading this.
type inspectItem struct {
	Ref     string       `json:"ref"`
	Name    string       `json:"name"`
	Group   string       `json:"group"`
	Status  string       `json:"status"`
	Type    string       `json:"type,omitempty"`
	Engine  state.Engine `json:"engine"`
	Network string       `json:"network"`
	Created string       `json:"created,omitempty"`
	Yolo    bool         `json:"yolo"`
	Solo    bool         `json:"solo"`
	// Port is the host loopback port publishing this sandbox's sshd.
	Port int `json:"port,omitempty"`
	// IP, CPUs and MemMiB are firecracker-only and omitted for podman.
	IP     string `json:"ip,omitempty"`
	CPUs   int    `json:"cpus,omitempty"`
	MemMiB int    `json:"mem,omitempty"`
	// AgentLogins are the agents whose host login was inherited at create.
	AgentLogins []string `json:"agentlogins,omitempty"`
	Snapshots   []string `json:"snapshots,omitempty"`
	// Repos is the reason this command exists: each --repo checkout with the
	// branch it commits to and that `fetch` lands on in the host source repo.
	Repos []inspectRepo `json:"repos,omitempty"`
	// ImageStores are the shared read-only stores mounted into the sandbox.
	ImageStores []string `json:"imagestores,omitempty"`
}

type inspectRepo struct {
	Dir    string `json:"dir"`    // ~/<dir> inside the sandbox
	Source string `json:"source"` // the host repository it was cloned from
	Branch string `json:"branch"` // what to fetch, spelled by this repo
}

func newInspectCmd(app *App) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "inspect <name>",
		Short:             "Show everything recorded about one sandbox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := app.resolve(args[0])
			if err != nil {
				return err
			}
			item := app.inspectOne(cmd.Context(), in)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(item)
			}
			return writeInspectTable(cmd.OutOrStdout(), item)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print stable machine-readable JSON")
	return cmd
}

func (a *App) inspectOne(ctx context.Context, in *state.Instance) inspectItem {
	// The same status lookup `ls` uses, so the two can never disagree about
	// whether a sandbox is running.
	status := a.engineDepsFor(in.Group).Statuses(ctx, []*state.Instance{in})
	item := inspectItem{
		Ref: engine.Qualify(in), Name: in.Name, Group: in.Group,
		Status: status[engine.Qualify(in)], Type: in.Type, Engine: in.Engine,
		Network: state.NetworkName(in.Group), Created: in.Created,
		Yolo: in.Yolo, Solo: in.Solo, Port: in.Port,
		IP: in.FCIP, CPUs: in.CPUs, MemMiB: in.MemMiB,
		AgentLogins: in.AgentLogins, Snapshots: in.Snapshots,
		ImageStores: in.Shared,
	}
	for _, rc := range in.RepoClones {
		item.Repos = append(item.Repos, inspectRepo{Dir: rc.Dir, Source: rc.Source, Branch: rc.Branch})
	}
	return item
}

func writeInspectTable(out io.Writer, item inspectItem) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	row := func(k string, v any) { fmt.Fprintf(tw, "%s\t%v\n", k, v) }
	row("REF", item.Ref)
	row("GROUP", item.Group)
	row("STATUS", item.Status)
	row("TYPE", item.Type)
	row("ENGINE", item.Engine)
	row("NETWORK", item.Network)
	if item.Port != 0 {
		row("SSH PORT", item.Port)
	}
	if item.IP != "" {
		row("IP", item.IP)
	}
	if item.CPUs != 0 {
		row("CPUS", item.CPUs)
	}
	if item.MemMiB != 0 {
		row("MEM MIB", item.MemMiB)
	}
	row("YOLO", yn(item.Yolo))
	row("SOLO", yn(item.Solo))
	if len(item.AgentLogins) > 0 {
		row("AGENT LOGINS", strings.Join(item.AgentLogins, ", "))
	}
	if len(item.ImageStores) > 0 {
		row("IMAGE STORES", strings.Join(item.ImageStores, ", "))
	}
	for _, sn := range item.Snapshots {
		row("SNAPSHOT", sn)
	}
	// One line per repo, carrying the branch: this is what a caller came for.
	for _, r := range item.Repos {
		row("REPO", fmt.Sprintf("~/%s  branch=%s  source=%s", r.Dir, r.Branch, r.Source))
	}
	return tw.Flush()
}
