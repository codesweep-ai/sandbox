package fcconfig

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestBuildDrives(t *testing.T) {
	tests := []struct {
		name    string
		spec    Spec
		wantIDs []string
	}{
		{
			name:    "rootfs and seed only",
			spec:    Spec{RootfsPath: "/r.ext4", SeedPath: "/s.ext4"},
			wantIDs: []string{"rootfs", "seed"},
		},
		{
			name: "two repos one snapshot",
			spec: Spec{
				RootfsPath:    "/r.ext4",
				SeedPath:      "/s.ext4",
				RepoDisks:     []string{"/repo1.ext4", "/repo2.ext4"},
				SnapshotDisks: []string{"/snap1.ext4"},
			},
			wantIDs: []string{"rootfs", "seed", "repo1", "repo2", "snap1"},
		},
		{
			name: "all categories in fixed order",
			spec: Spec{
				RootfsPath:    "/r.ext4",
				SeedPath:      "/s.ext4",
				RepoDisks:     []string{"/repo1.ext4"},
				SnapshotDisks: []string{"/snap1.ext4", "/snap2.ext4"},
				StoreDisks:    []string{"/store1.ext4"},
			},
			wantIDs: []string{"rootfs", "seed", "repo1", "snap1", "snap2", "store1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Build(tc.spec)
			gotIDs := make([]string, len(c.Drives))
			for i, d := range c.Drives {
				gotIDs[i] = d.DriveID
			}
			if !reflect.DeepEqual(gotIDs, tc.wantIDs) {
				t.Fatalf("drive ids = %v, want %v", gotIDs, tc.wantIDs)
			}
			// rootfs is the only root + read-write device.
			for _, d := range c.Drives {
				if d.DriveID == "rootfs" {
					if !d.IsRootDevice || d.IsReadOnly {
						t.Errorf("rootfs = %+v, want root+rw", d)
					}
				} else if d.IsRootDevice || !d.IsReadOnly {
					t.Errorf("%s = %+v, want non-root+ro", d.DriveID, d)
				}
			}
		})
	}
}

func goldenSpec() Spec {
	return Spec{
		KernelPath:    "/cache/vmlinux.elf",
		InitrdPath:    "/cache/initrd.img",
		RootfsPath:    "/inst/rootfs.ext4",
		SeedPath:      "/inst/seed.ext4",
		TapName:       "fdt200",
		MAC:           "02:fc:0a:59:00:c8",
		VsockPath:     "/inst/vm.vsock",
		VCPUs:         4,
		MemMiB:        4096,
		RepoDisks:     []string{"/inst/repo1.ext4", "/inst/repo2.ext4"},
		SnapshotDisks: []string{"/inst/snap1.ext4"},
	}
}

func TestGoldenRoundTrip(t *testing.T) {
	c := Build(goldenSpec())

	b, err := c.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal into Config: %v", err)
	}

	wantIDs := []struct {
		id   string
		root bool
		ro   bool
	}{
		{"rootfs", true, false},
		{"seed", false, true},
		{"repo1", false, true},
		{"repo2", false, true},
		{"snap1", false, true},
	}
	if len(got.Drives) != len(wantIDs) {
		t.Fatalf("drive count = %d, want %d", len(got.Drives), len(wantIDs))
	}
	for i, w := range wantIDs {
		d := got.Drives[i]
		if d.DriveID != w.id || d.IsRootDevice != w.root || d.IsReadOnly != w.ro {
			t.Errorf("drive[%d] = %+v, want id=%s root=%v ro=%v", i, d, w.id, w.root, w.ro)
		}
	}

	if got.BootSource.BootArgs != DefaultBootArgs {
		t.Errorf("boot_args = %q, want %q", got.BootSource.BootArgs, DefaultBootArgs)
	}
	if got.BootSource.BootArgs != "console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/fc-init "+
		"page_reporting.page_reporting_order=0 quiet" {
		t.Errorf("boot_args literal mismatch: %q", got.BootSource.BootArgs)
	}

	if got.Vsock.GuestCID != 3 {
		t.Errorf("guest_cid = %d, want 3", got.Vsock.GuestCID)
	}

	if len(got.NetworkInterfaces) != 1 {
		t.Fatalf("network-interfaces count = %d, want 1", len(got.NetworkInterfaces))
	}
	ni := got.NetworkInterfaces[0]
	if ni.IfaceID != "eth0" || ni.HostDevName != "fdt200" || ni.GuestMAC != "02:fc:0a:59:00:c8" {
		t.Errorf("network interface = %+v", ni)
	}

	if got.MachineConfig.VCPUCount != 4 || got.MachineConfig.MemSizeMiB != 4096 {
		t.Errorf("machine-config = %+v, want 4/4096", got.MachineConfig)
	}
}

func TestTopLevelKeys(t *testing.T) {
	c := Build(goldenSpec())
	b, err := c.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}

	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{"balloon", "boot-source", "drives", "machine-config", "network-interfaces", "vsock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
}

// TestBalloonEnablesFreePageReporting: the balloon exists only to carry free
// page reporting, so it must be inert as a balloon (amount 0, no deflate-on-oom)
// and reporting must be on. Reporting is pre-boot only — a config that omits it
// cannot be corrected on a running VM.
func TestBalloonEnablesFreePageReporting(t *testing.T) {
	got := Build(Spec{VCPUs: 2, MemMiB: 1024})
	if got.Balloon == nil {
		t.Fatal("no balloon device: free page reporting cannot be enabled later")
	}
	if !got.Balloon.FreePageReporting {
		t.Error("free_page_reporting must be true")
	}
	if got.Balloon.AmountMiB != 0 || got.Balloon.DeflateOnOOM {
		t.Errorf("the balloon must stay inert, got %+v", *got.Balloon)
	}
}
