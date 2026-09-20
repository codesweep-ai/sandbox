package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoteDeploysTheDriverItShipsWith: a remote tool copies its turn driver into the machine
// it drives, on every turn, over whatever driver is there. It used to take that driver from
// the caller's ~/.local/bin, wherever the tool itself was installed. A caller with an older
// install there, running a newer tool from a pinned directory, then replaced the new image's
// driver with the old one, and the machine ran a driver that matched neither its image nor
// the tool that started the turn. Nothing reported it. The tool and its driver ship as a
// pair, so the driver beside the tool is the one that goes.
func TestRemoteDeploysTheDriverItShipsWith(t *testing.T) {
	skipUnlessLinux(t)
	for _, family := range []string{"codex", "claude", "opencode"} {
		t.Run(family, func(t *testing.T) {
			remote, driver := "cs-"+family+"-remote", "cs-"+family+"-turn"
			home, bin := agentHome(t, ".cs-"+family+"-remote")

			// The install the tool runs from, holding the pair.
			install := t.TempDir()
			src, err := os.ReadFile(agentTool(remote))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(install, remote), src, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(install, driver), []byte("#!/bin/sh\n# PAIRED\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			// An older install in the caller's home.
			stale := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(stale, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stale, driver), []byte("#!/bin/sh\n# STALE\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			sent := filepath.Join(t.TempDir(), "sent")
			writeStub(t, bin, "scp", "#!/bin/sh\nfor a in \"$@\"; do [ -f \"$a\" ] && cat \"$a\" >> \"$SENT\"; done\nexit 0\n")

			cmd := exec.Command(filepath.Join(install, remote), "-H", "box", "--new", "--name", "pairing", "say ok")
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin", "SENT="+sent)
			_, _ = cmd.CombinedOutput() // the turn itself goes nowhere: ssh is a stub

			got, _ := os.ReadFile(sent)
			if !strings.Contains(string(got), "PAIRED") || strings.Contains(string(got), "STALE") {
				t.Fatalf("%s deployed %q; want the driver it ships with, and never the caller's older one", remote, got)
			}

			// With no driver beside it, the tool falls back to the caller's install, as before.
			if err := os.Remove(filepath.Join(install, driver)); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(sent)
			cmd = exec.Command(filepath.Join(install, remote), "-H", "box", "--new", "--name", "pairing2", "say ok")
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":/usr/bin:/bin", "SENT="+sent)
			_, _ = cmd.CombinedOutput()
			if got, _ := os.ReadFile(sent); !strings.Contains(string(got), "STALE") {
				t.Fatalf("%s with no driver beside it deployed %q; want the one in ~/.local/bin", remote, got)
			}
		})
	}
}
