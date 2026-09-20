package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/codesweep-ai/sandbox/internal/lend"
	"github.com/codesweep-ai/sandbox/internal/state"
)

// newLenderFaultCmd arms a provider failure for one sandbox's lent calls.
//
// It writes a file beside the sandbox's loans and talks to no lender: the
// group's lender reads the file on the sandbox's next call. That keeps a fault
// where an origin is, in the operator's hands and out of any request's reach.
func newLenderFaultCmd(app *App) *cobra.Command {
	var f lend.Fault
	var bodyFile string
	var clearAll bool
	cmd := &cobra.Command{
		Use:   "fault <name>",
		Short: "Fail a sandbox's next lent model calls on purpose, to test what drives it",
		Long: "Answer a sandbox's next lent model calls with a provider failure, without calling the\n" +
			"provider. Use it to see what a turn driver, a wrapper or a fleet harness does under a\n" +
			"throttle, an outage, an expired credential or a hang. With no flags, list what is armed.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: app.completeSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, in, err := app.engineFor(args[0])
			if err != nil {
				return err
			}
			dir := state.Dir(app.InstDir, in.Group, in.Name)
			out := cmd.OutOrStdout()
			armed, err := lend.ReadFaults(dir)
			if err != nil {
				return err
			}
			arming := cmd.Flags().Changed("status") || cmd.Flags().Changed("hang")
			switch {
			case clearAll:
				if err := lend.WriteFaults(dir, nil); err != nil {
					return err
				}
				fmt.Fprintf(out, "cleared %d fault(s) on %s\n", len(armed), Ref(in))
				return nil
			case !arming:
				if len(armed) == 0 {
					fmt.Fprintf(out, "no fault is armed on %s\n", Ref(in))
				}
				for _, a := range armed {
					fmt.Fprintf(out, "%s  %s\n", a.ID, describeFault(a))
				}
				return nil
			}

			// A fault is answered by the lender, so it only ever reaches a
			// sandbox that borrows. On any other it would sit there doing
			// nothing, which reads as a test that passed.
			slots := loanSlots(app.InstDir, in.Group, in.Name)
			if len(slots) == 0 {
				return fmt.Errorf("%s borrows no credential, so none of its calls pass through a lender: "+
					"create it with --lend-api-key or --lend-agent-login", Ref(in))
			}
			if f.Slot != "" && !slices.Contains(slots, f.Slot) {
				return fmt.Errorf("%s does not borrow %s; it borrows %s", Ref(in), f.Slot, strings.Join(slots, ", "))
			}
			if bodyFile != "" {
				body, err := os.ReadFile(bodyFile)
				if err != nil {
					return err
				}
				f.Body = string(body)
			}
			var id [4]byte
			if _, err := rand.Read(id[:]); err != nil {
				return err
			}
			f.ID = hex.EncodeToString(id[:])
			if err := f.Validate(); err != nil {
				return err
			}
			if err := lend.WriteFaults(dir, append(armed, f)); err != nil {
				return err
			}
			fmt.Fprintf(out, "armed %s on %s: %s\n", f.ID, Ref(in), describeFault(f))
			return nil
		},
	}
	cmd.Flags().IntVar(&f.Status, "status", 0, "the status to answer with, such as 429, 503 or 401")
	cmd.Flags().IntVar(&f.Count, "count", 1, "how many calls to fail before traffic flows again")
	cmd.Flags().StringVar(&f.Slot, "slot", "", "fail one borrowed credential's calls only (default: every one the sandbox borrows)")
	cmd.Flags().IntVar(&f.RetryAfter, "retry-after", 0, "send a Retry-After header of this many seconds")
	cmd.Flags().StringVar(&f.Hang, "hang", "", "hold each call this long first, such as 90s; with no --status the call is then dropped unanswered")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "answer with this file as the body, for a provider that reports a failure inside a 200 stream")
	cmd.Flags().StringVar(&f.ContentType, "content-type", "", "the Content-Type of --body-file (default application/json)")
	cmd.Flags().BoolVar(&clearAll, "clear", false, "remove every fault armed on the sandbox")
	return cmd
}

func describeFault(f lend.Fault) string {
	var parts []string
	if f.Hang != "" {
		parts = append(parts, "hang "+f.Hang)
	}
	if f.Status != 0 {
		parts = append(parts, fmt.Sprintf("answer %d", f.Status))
	} else {
		parts = append(parts, "drop the connection")
	}
	slot := "every slot"
	if f.Slot != "" {
		slot = f.Slot
	}
	return fmt.Sprintf("%s, next %d call(s) on %s", strings.Join(parts, ", then "), f.Count, slot)
}
