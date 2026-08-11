// Package fcconfig builds a Firecracker microVM configuration file (run.json)
// from a typed spec.
//
// The emitted document matches Firecracker's API v1.16 config-file schema:
//
//	{
//	  "boot-source":        { "kernel_image_path", "initrd_path", "boot_args" },
//	  "drives":             [ { "drive_id", "path_on_host", "is_root_device", "is_read_only" }, ... ],
//	  "network-interfaces": [ { "iface_id", "host_dev_name", "guest_mac" } ],
//	  "vsock":              { "guest_cid", "uds_path" },
//	  "machine-config":     { "vcpu_count", "mem_size_mib" }
//	}
package fcconfig

import (
	"encoding/json"
	"os"
)

// DefaultBootArgs is the kernel command line used for every microVM.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/fc-init quiet"

// GuestCID is the fixed vsock context ID assigned to every guest.
const GuestCID = 3

// BootSource describes the kernel, initrd and boot arguments.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	InitrdPath      string `json:"initrd_path"`
	BootArgs        string `json:"boot_args"`
}

// Drive is a single virtio-block device attached to the microVM.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface is a single virtio-net tap device.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

// Vsock describes the host<->guest vsock channel.
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// MachineConfig describes the VM resource allocation.
type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

// Config is the full Firecracker run.json document.
type Config struct {
	BootSource        BootSource         `json:"boot-source"`
	Drives            []Drive            `json:"drives"`
	NetworkInterfaces []NetworkInterface `json:"network-interfaces"`
	Vsock             Vsock              `json:"vsock"`
	MachineConfig     MachineConfig      `json:"machine-config"`
}

// Spec is the input to Build: the host paths, network identity and resource
// sizing for one microVM, plus the ordered lists of auxiliary read-only disks.
type Spec struct {
	KernelPath string
	InitrdPath string
	RootfsPath string
	SeedPath   string
	TapName    string
	MAC        string
	VsockPath  string

	VCPUs  int
	MemMiB int

	// RepoDisks, SnapshotDisks and StoreDisks are host paths for the
	// repeatable auxiliary read-only disks, appended to the drive list in
	// this fixed order: repos, then snapshots, then image stores. Each list
	// is numbered from 1 within its category, yielding drive_ids repo1,
	// repo2, ..., snap1, ..., store1, ....
	RepoDisks     []string
	SnapshotDisks []string
	StoreDisks    []string
}

// Build assembles a Config from a Spec. The drive order is fixed: rootfs
// (root, read-write), seed (read-only), then repo, snapshot and image-store
// disks — each read-only and numbered from 1 within its category.
func Build(s Spec) *Config {
	drives := make([]Drive, 0, 2+len(s.RepoDisks)+len(s.SnapshotDisks)+len(s.StoreDisks))
	drives = append(drives,
		Drive{DriveID: "rootfs", PathOnHost: s.RootfsPath, IsRootDevice: true, IsReadOnly: false},
		Drive{DriveID: "seed", PathOnHost: s.SeedPath, IsRootDevice: false, IsReadOnly: true},
	)
	appendAux := func(prefix string, paths []string) {
		for i, p := range paths {
			drives = append(drives, Drive{
				DriveID:      prefix + itoa(i+1),
				PathOnHost:   p,
				IsRootDevice: false,
				IsReadOnly:   true,
			})
		}
	}
	appendAux("repo", s.RepoDisks)
	appendAux("snap", s.SnapshotDisks)
	appendAux("store", s.StoreDisks)

	return &Config{
		BootSource: BootSource{
			KernelImagePath: s.KernelPath,
			InitrdPath:      s.InitrdPath,
			BootArgs:        DefaultBootArgs,
		},
		Drives: drives,
		NetworkInterfaces: []NetworkInterface{
			{IfaceID: "eth0", HostDevName: s.TapName, GuestMAC: s.MAC},
		},
		Vsock: Vsock{
			GuestCID: GuestCID,
			UDSPath:  s.VsockPath,
		},
		MachineConfig: MachineConfig{
			VCPUCount:  s.VCPUs,
			MemSizeMiB: s.MemMiB,
		},
	}
}

// JSON marshals the Config as indented JSON.
func (c *Config) JSON() ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}

// WriteFile marshals the Config and writes it to path at 0600 (matching the
// per-instance seed-file convention).
func (c *Config) WriteFile(path string) error {
	b, err := c.JSON()
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ReadFile parses a previously written run.json. It is the reverse of
// WriteFile, and exists so callers can ask what an instance is actually
// configured to boot — notably which host paths it attaches as drives — without
// re-deriving it from a Spec.
func ReadFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// itoa converts a small non-negative int to its decimal string without pulling
// in strconv; drive counts are tiny so this stays simple.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
