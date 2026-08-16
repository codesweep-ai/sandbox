package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeRunJSON drops a minimal run.json with the given guest RAM, which is what
// cgroupWrapper derives its default ceiling from.
func writeRunJSON(t *testing.T, dir string, memMiB int) {
	t.Helper()
	body := `{"machine-config":{"vcpu_count":2,"mem_size_mib":` + itoaTest(memMiB) + `}}`
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func argvHas(argv []string, want string) bool {
	return slices.Contains(argv, want)
}

// TestCgroupWrapperDerivesCeiling: with no explicit override the ceiling comes
// from the instance's own mem_size_mib plus headroom, so it sits above anything
// the guest can reach and acts as a backstop rather than a throttle.
func TestCgroupWrapperDerivesCeiling(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, 4096)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "")
	t.Setenv("CS_SANDBOX_FC_MEMORY_MAX", "")
	t.Setenv("CS_SANDBOX_FC_MEMORY_SWAP_MAX", "")

	argv := cgroupWrapper(dir, &bytes.Buffer{})
	if len(argv) == 0 {
		t.Skip("systemd-run not present on this host")
	}
	want := "MemoryMax=" + itoaTest(4096+cgroupHeadroomMiB) + "M"
	if !argvHas(argv, want) {
		t.Errorf("argv %v does not contain %q", argv, want)
	}
	if !argvHas(argv, "MemorySwapMax=0") {
		t.Errorf("swap should default to 0 (on a zram host it is RAM): %v", argv)
	}
	// memory.high is a silent hang, not a throttle — it must never be set.
	for _, a := range argv {
		if strings.Contains(a, "MemoryHigh") {
			t.Errorf("MemoryHigh must not be set: %v", argv)
		}
	}
}

// TestCgroupWrapperExplicitOverride: an operator packing sandboxes past the sum
// of their ceilings sets the limit directly, and it is used verbatim.
func TestCgroupWrapperExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, 4096)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "")
	t.Setenv("CS_SANDBOX_FC_MEMORY_MAX", "1500M")
	t.Setenv("CS_SANDBOX_FC_MEMORY_SWAP_MAX", "2G")

	argv := cgroupWrapper(dir, &bytes.Buffer{})
	if len(argv) == 0 {
		t.Skip("systemd-run not present on this host")
	}
	if !argvHas(argv, "MemoryMax=1500M") || !argvHas(argv, "MemorySwapMax=2G") {
		t.Errorf("explicit limits not honoured: %v", argv)
	}
}

// TestCgroupWrapperKillSwitch: the whole mechanism can be turned off.
func TestCgroupWrapperKillSwitch(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, 4096)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "1")

	if argv := cgroupWrapper(dir, &bytes.Buffer{}); argv != nil {
		t.Errorf("CS_SANDBOX_FC_NO_CGROUP should disable wrapping, got %v", argv)
	}
}

// TestCgroupWrapperDegradesWithoutSession: a host with no systemd user session
// must still boot microVMs — unwrapped, with a warning — rather than failing.
func TestCgroupWrapperDegradesWithoutSession(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, 4096)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	var warn bytes.Buffer
	if argv := cgroupWrapper(dir, &warn); argv != nil {
		t.Errorf("expected no wrapper without a user session, got %v", argv)
	}
	if !strings.Contains(warn.String(), "without a memory cgroup") {
		t.Errorf("the absence of the safety net should be visible, got %q", warn.String())
	}
}

// TestCgroupWrapperUnreadableConfig: if the sizing is unknown we would rather
// run unwrapped than impose a ceiling picked out of the air.
func TestCgroupWrapperUnreadableConfig(t *testing.T) {
	dir := t.TempDir() // no run.json
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "")
	t.Setenv("CS_SANDBOX_FC_MEMORY_MAX", "")

	if argv := cgroupWrapper(dir, &bytes.Buffer{}); argv != nil {
		t.Errorf("expected no wrapper when mem sizing is unknown, got %v", argv)
	}
}

// TestCgroupWrapperUnitNamesAreUnique: a scope whose process is OOM-killed stays
// in `failed` state and blocks reuse of its name, so every launch needs a fresh
// one or the second boot of a killed sandbox fails.
func TestCgroupWrapperUnitNamesAreUnique(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, 512)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("CS_SANDBOX_FC_NO_CGROUP", "")
	t.Setenv("CS_SANDBOX_FC_MEMORY_MAX", "")

	unit := func() string {
		for _, a := range cgroupWrapper(dir, &bytes.Buffer{}) {
			if strings.HasPrefix(a, "--unit=") {
				return a
			}
		}
		return ""
	}
	a, b := unit(), unit()
	if a == "" {
		t.Skip("systemd-run not present on this host")
	}
	if a == b {
		t.Errorf("unit name reused across launches: %q", a)
	}
}
