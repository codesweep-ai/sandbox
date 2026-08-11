package doctor

import (
	"os"
	"strconv"
	"strings"
)

// memoryGroup reports the host settings that decide what a fleet of microVMs
// costs in memory. All advisory: a host missing every one of them still runs
// sandboxes, just with a bigger footprint. It is here because each fails
// silently — KSM left off dedupes nothing while looking enabled, and reclaim on
// a zram host frees far less than the numbers suggest. See docs/firecracker.md.
//
// Every check reads /proc or /sys, so none of it needs privilege.
func memoryGroup() Group {
	g := Group{Title: "firecracker memory management (optional)"}
	ksmChecks(&g)
	swapChecks(&g)
	return g
}

func ksmChecks(g *Group) {
	run, err := readSysInt("/sys/kernel/mm/ksm/run")
	if err != nil {
		g.add(HM, "KSM unavailable in this kernel — sandboxes cannot share pages")
		return
	}
	if run != 1 {
		g.add(HM, "KSM off — sandboxes do not share identical pages; enable with:  "+
			"sudo sh -c 'echo 1 > /sys/kernel/mm/ksm/run'")
		return
	}
	sharing, _ := readSysInt("/sys/kernel/mm/ksm/pages_sharing")
	g.add(OK, "KSM on ("+strconv.Itoa(sharing*4/1024)+" MiB shared)")

	// ~20 MiB/s at the default, so a multi-GiB fleet dedupes minutes after it boots.
	if scan, err := readSysInt("/sys/kernel/mm/ksm/pages_to_scan"); err == nil && scan <= 100 {
		g.add(HM, "KSM scan rate is slow for a fleet — raise with:  "+
			"sudo sh -c 'echo 1000 > /sys/kernel/mm/ksm/pages_to_scan'")
	}
}

func swapChecks(g *Group) {
	data, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return
	}
	switch zram, disk := countSwaps(string(data)); {
	case zram == 0 && disk == 0:
		g.add(HM, "no swap — the host cannot reclaim a sandbox's memory under pressure")
	case disk == 0:
		g.add(HM, "swap is zram — reclaim compresses in RAM rather than freeing it")
	default:
		g.add(OK, "disk swap present")
	}
}

// countSwaps classifies the entries of /proc/swaps. The distinction is the
// whole point: a zram device is RAM, so swapping to it compresses pages in
// place, while a disk device genuinely hands memory back.
func countSwaps(procSwaps string) (zram, disk int) {
	lines := strings.Split(procSwaps, "\n")
	if len(lines) > 0 {
		lines = lines[1:] // header
	}
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if strings.HasPrefix(f[0], "/dev/zram") {
			zram++
		} else {
			disk++
		}
	}
	return zram, disk
}

func readSysInt(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
