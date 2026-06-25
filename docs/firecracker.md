# sandbox - Firecracker microVM engine

`cs-sandbox --engine firecracker` - the **default** engine on a Linux/KVM host - runs each sandbox
as a Firecracker **microVM** instead of a Podman container, reusing the same OCI image, the same
`cs-sandbox` CLI, and the same SSH + repo capabilities. This document covers what is specific to the
microVM engine; the cross-engine model (trust, the generic image, agent tooling) lives in
[`design.md`](design.md).

## Why a microVM

A separate guest **kernel** per sandbox replaces the shared host kernel of a container, removing
the container engine's main residual weakness - host-kernel attack surface. (The container path
keeps that surface narrow with a scaled-down cap set + seccomp; a VM removes it entirely.)
Especially valuable for the **agent** type. A bonus falls out: inside a real VM you are real root on
a real kernel, so the whole nested-podman apparatus the container engine needs (scaled-down caps,
`--userns=keep-id`, the `sudo` Podman wrapper - see
[`podman.md`](podman.md#nested-podman)) is **unnecessary**; Podman
just runs.

The cost: a microVM has no live host-directory share (no `virtio-fs`), so the rootfs and shared
directories are delivered as block devices, not live mounts.

## Prerequisites

All rootless - no host `sudo` to *run* it - but the engine shells out to host packages, which
`cs-sandbox` preflight-checks (failing with an actionable install line). The Firecracker binary is
auto-downloaded (SHA256-verified) to `.fc-cache/`. **`./cs-sandbox doctor` checks all of the below
and prints the exact fix for anything missing.**

- **Packages:** `passt` (Podman's rootless uplink, which VMs share), `dnsmasq` (the forwarding
  VM-name resolver), `fakeroot` + `e2fsprogs` (build the ext4 disks), `socat` + `python3` (the
  host→VM port/vsock bridges), `shadow-utils`/`uidmap` (`newuidmap`, for Podman's rootless userns),
  `curl`, `git`. The preflight detects `dnf` vs `apt` and prints the right names:

  ```bash
  # Fedora
  sudo dnf install podman passt dnsmasq fakeroot e2fsprogs socat python3 shadow-utils curl git
  # Ubuntu / Debian  (uidmap not shadow-utils, dnsmasq-base not dnsmasq)
  sudo apt install podman passt dnsmasq-base fakeroot e2fsprogs socat python3 uidmap curl git
  ```

- **Host kernel:** a Linux host with KVM (`/dev/kvm`). x86_64 only.

  ```bash
  sudo usermod -aG kvm "$USER"            # grant /dev/kvm access (log out / back in afterward)
  ```

- **Rootless userns:** your user needs a subuid/subgid range (the entrypoint sub-divides it for
  nested Podman):

  ```bash
  grep "^$USER:" /etc/subuid /etc/subgid  # must return a line for each file
  ```

- **Ubuntu 24.04+ only:** if Podman's rootless network namespace fails to start, allow unprivileged
  user namespaces:

  ```bash
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  ```

> The base **Podman** install (and, on macOS, `podman machine init && podman machine start`) is the
> one prerequisite shared with the Podman engine - it builds the image and provides the network
> fabric on every host. The Firecracker engine is Linux/KVM-only; on macOS / non-KVM hosts sandboxes
> use Podman automatically.

## Per-sandbox anatomy

Each microVM is assembled from a per-image boot **kernel**, a stack of **block devices**, and an
**init** that wires it together - built and launched entirely rootless:

```
  host (rootless)
  ┌──────────────────────────────────────────────────────────────┐
  │ cs-sandbox · Firecracker engine                              │
  │                                                              │
  │ cached per image:  vmlinux.elf · initrd · base rootfs        │
  │ per sandbox:       rootfs.ext4 (rw, reflink) · seed.ext4 (ro)│
  │                    · repo / snap / store .ext4 (ro)          │
  └───────────────────────────────┬──────────────────────────────┘
        podman unshare --rootless-netns   (launch)
        block devices  →  vda / vdb / vdc …
                                  ▼
  microVM (Firecracker + KVM)
  ┌──────────────────────────────────────────────────────────────┐
  │ PID1 = /fc-init  (real root):                                │
  │   • mount disks; read RO seed                                │
  │   • create user; seed home + ~/.ssh; install agent creds     │
  │   • eth0 static; sshd :22                                    │
  │   • --repo: clone --shared                                   │
  └──────────────────────────────────────────────────────────────┘
```

### Guest kernel

Firecracker x86_64 boots an **uncompressed ELF `vmlinux` + an initrd** (not a bzImage, and not the
PVH boot protocol). Two ways to obtain the `vmlinux`:

- **`CS_SANDBOX_FC_KERNEL=fedora` (default):** build the kernel **from the sandbox image** in a
  throwaway container, **version-pinned** via `CS_SANDBOX_FC_KVER`. The same kernel then boots on any
  host (Fedora, Ubuntu, …) with **no** dependency on the host's `dracut` or `/boot`, and microVMs are
  reproducible rather than tracking whatever kernel is latest. (`extract-vmlinux` is fetched inside
  that build container; bumping `CS_SANDBOX_FC_KVER` rebuilds the cached artifacts.)
- **`CS_SANDBOX_FC_KERNEL=host`:** reuse the running host kernel (`uname -r`) - smaller, auto-tracks
  host upgrades, but **Fedora-host only**: it needs `dracut` and a readable `/boot/vmlinuz-<ver>`
  that `extract-vmlinux` (downloaded to `.fc-cache/`) can unwrap to an ELF image.

Either way the cached artifacts are `vmlinux.elf` + `initrd.img` + `modules.tar` (+ `kver`) under
`.fc-cache/`.

### Disks

Everything else reaches the VM as a virtio-block device. The drives are emitted to `run.json` in a
**fixed order**, and the guest init walks the optional ones with a single device-letter cursor in
that same order - so host append-order and guest consume-order must match:

| device | role | mode |
|---|---|---|
| `/dev/vda` | **rootfs** | rw |
| `/dev/vdb` | **seed** | ro |
| `/dev/vdc…` | **repo** (per `--repo`), then **snapshot** (per `--snapshot`), then **image-store** (per `--image-store`) | ro |

- **rootfs:** built once per image from `podman export` through a `fakeroot` pipeline (+ the baked
  guest init at `/fc-init` + guest `/lib/modules`); each sandbox gets a `cp --reflink=auto` copy
  (near-free CoW on btrfs/xfs, a full copy elsewhere). Holds `/home/<user>`, so it persists across
  stop/start.
- **seed:** the per-sandbox config + credentials as a small RO ext4 (next section).
- **repo / snapshot / image-store:** content-addressed cached RO ext4 disks, reflink-copied per
  sandbox. `--repo` is a bare clone the guest then `clone --shared`s (see
  [`repo-sharing.md`](repo-sharing.md)); `--snapshot` is a frozen directory;
  `--image-store` is a shared Podman store wired into the guest Podman's `additionalimagestores` (see
  [`design.md`](design.md#shared-image-stores)). Cache keys: repo =
  `sha256`(ref tips + HEAD), image-store = `sha256`(`images.json` + `layers.json`), each 40 hex;
  disks unused for `CS_SANDBOX_FC_REPO_CACHE_TTL_DAYS` (default 14) are pruned.

### Seed assembly

The seed is built in two stages, then packed into `seed.ext4` with `fakeroot mke2fs -d`:

1. `build_seed` (shared with the Podman engine) writes `instances/<name>/seed/`: `authorized_keys`,
   the tier key, stable `host_keys/`, the sandbox-scoped `ssh_config`, `host_hosts` (reach the host
   by name), the host `~/.ssh` snapshot (user VMs), and `claude/` + `codex/` credentials (including
   the API-key/cloud `env` + `creds/` when present).
2. `fc_cmd_create` copies those into an `fc-seed/` dir and adds **`cs-sandbox.conf`** - the identity +
   network contract: `CS_SANDBOX_USER`/`UID`/`GID`/`HOSTNAME` (the bare sandbox name), `TYPE`,
   `YOLO`, `IP`, `GW`, `DNS` - plus the `repos` / `snapshots` / `imagestores` manifests.

The guest mounts it read-only at `/run/cs-sandbox-seed` and sources `cs-sandbox.conf` first.

### Guest init (PID1)

A kernel boots an init, not an entrypoint, so `/fc-init` replaces the container `ENTRYPOINT` (it
skips the keep-id / runtime-user dance - the VM is genuinely root with its own uids). In order:

1. mount `proc`/`sys`/`devtmpfs`/`cgroup2`/`devpts`; `modprobe vsock`;
2. mount the seed (`/dev/vdb`); source `cs-sandbox.conf`;
3. create the developer user + a NOPASSWD sudoers entry;
4. `modprobe virtio_net`, wait for the NIC, set the seeded static IP/route/`resolv.conf`;
5. write `/etc/hosts` (localhost, self, and append `host_hosts` so `ssh <hostname>` reaches the host)
   and `/etc/gai.conf` (prefer IPv4, since the net is v4-only but DNS returns AAAA - see the design
   doc's "Reaching the host by name"); open `ping_group_range` for unprivileged ICMP;
6. **first boot:** seed `/home/<user>` from the image skeleton (`/sandbox/home`) + the host `~/.ssh`
   snapshot; **every boot:** refresh the managed ssh material (authorized_keys, tier key, `ssh_config`
   → `config.d/cs-sandbox`, host keys); install Claude/Codex creds + onboarding/YOLO markers;
7. the `--repo` alternates-clone; RO-mount `--snapshot` / `--image-store` disks (device-letter cursor);
8. `sshd -p 22`; print `FC-VM-READY`; `exec socat VSOCK-LISTEN:22 … :22` as PID1.

Boot to ready is ~1-2 s.

## Networking - one unified fabric

Containers and VMs share **one** rootless L2 fabric - a dedicated Podman network `cs-sandbox-net` -
so they reach each other directly and by name across engines. Rather than a separate namespace, a VM
runs **inside Podman's own rootless network namespace** (entered with `podman unshare
--rootless-netns`), with a tap on the network's bridge (tap `fdt<lastoctet>`, MAC
`02:fc:0a:59:00:<lastoctet>`) and a static address from the **high end of the subnet**
(`<prefix>.200-.250`, above the low addresses netavark hands containers, so no clash).

```
  Podman rootless netns
  ┌──────────────────────────────────────────────────────────────┐
  │ bridge   10.89.0.1   (aardvark gateway)                      │
  │   ├── dnsmasq               10.89.0.53                       │
  │   ├── keepalive container                                    │
  │   ├── VM eth0               10.89.0.200   (tap fdt200)       │
  │   └── container             10.89.0.3                        │
  └──────────────▲───────────────────────────────▲───────────────┘
                 │                               ┊  (veth enslaved to bridge)
                 │  ssh: socat + unix-socket hop (host → VM)
  host root netns
  ┌──────────────┴───────────────────────────────┴───────────────┐
  │ host processes (ssh, curl)        cs-sandbox veth            │
  │                                   10.89.0.251 (host-route)   │
  └──────────────────────────────────────────────────────────────┘
```

(The `10.89.x` addresses are illustrative - `cs-sandbox` reads back whatever Podman assigns
`cs-sandbox-net`.)

Two helpers keep the fabric usable independent of user containers:

- **keepalive container** (`cs-sandbox-net-keepalive`, hidden from `ls`): netavark builds and tears
  down the bridge + aardvark-dns around *running containers*, so a lone VM would otherwise lose its
  bridge when the last container stops. The keepalive is a do-nothing container pinning the netns +
  bridge + aardvark.
- **forwarding dnsmasq** on a secondary bridge IP `<prefix>.53` (`--bind-interfaces
  --listen-address=<.53> --no-hosts --no-resolv --server=<gw> --hostsdir=<dir>`, run as userns-root so
  it can traverse a `750` home to re-read the hostsdir): serves VM names from an auto-reloading
  `--hostsdir` (`cs-sandbox` writes `<name> → ip` + SIGHUPs on create, drops it on destroy) and
  forwards everything else to aardvark. Liveness verifies the pid really is *our* dnsmasq on *our*
  address, so a leftover can't masquerade as healthy.

**Name resolution across engines.** VM → anything: the VM's resolver is the dnsmasq (VM names local,
the rest forwarded to aardvark). Container → VM: Podman pins a container's `resolv.conf` to aardvark
but records its `--dns` servers and forwards misses to them - so `cs-sandbox create` passes
`--dns <dnsmasq>` and aardvark forwards an unknown VM name to our dnsmasq, which answers it.

**Outbound** is Podman's own pasta uplink - VMs NAT out like containers. The fabric comes up lazily
on the first sandbox and is reclaimed only when no VM runs **and** no `cs-sandbox` container besides
the keepalive remains **and** host-route is off.

### host → VM ssh

The host can't address the rootless netns directly, so `ssh <name>` reaches the guest via a published
host port bridged with a **unix socket** (sockets ignore network namespaces): a host-side `socat`
binds the port (`~/.ssh/config.d/cs-sandbox-fc` → `HostName 127.0.0.1` / `Port N` / `HostKeyAlias`)
and relays through `fwd.sock` to a per-VM `socat` inside the netns that connects to the guest's `:22`.
Per-VM and lifecycle-tracked.

A Firecracker **vsock** is retained as a no-IP standby transport (it is *not* the routine ssh path):
guest CID 3; PID1 is `socat VSOCK-LISTEN:22 → TCP4:127.0.0.1:22`; the host side is a hybrid-vsock unix
socket `instances/<name>/vm.vsock`, and `fc/vsock-connect` speaks Firecracker's `CONNECT <port>` →
`OK <hostport>` handshake (wired as an ssh `ProxyCommand` in the generated config).

### Optional: reach sandboxes directly from the host (`host-route`)

The fabric deliberately keeps the host **out** of the rootless netns, so bare `ping <name>` / `curl
<name>:PORT` from the host don't work the way they do from a peer sandbox (which is inside the
fabric). `cs-sandbox host-route up` closes that gap in two one-time `sudo`'d steps - **off by
default, Linux-only, needs systemd-resolved**, and the only feature that uses `sudo`:

1. **a veth onto the subnet:** one end (`cs-sandbox`) stays in the host root netns at `<prefix>.251`,
   the peer (`cs-sandbox-ns`) is placed into the rootless netns (by PID - the userns blocks a bare
   `nsenter`) and enslaved to the bridge, giving the host a connected route to every sandbox and to
   the dnsmasq at `<prefix>.53`;
2. **DNS for `.cs.sandbox`:** point systemd-resolved at that dnsmasq for the suffix (`resolvectl dns
   cs-sandbox <.53>` + a routing-only `~cs.sandbox` domain scoped to the veth link); `host-route`
   publishes `<ip> <name>.cs.sandbox` into the hostsdir. So `ping`/`curl <name>.cs.sandbox` resolve
   through the fabric, for any protocol.

A **suffix** is unavoidable - systemd-resolved only routes a *suffixed* domain to a per-link resolver,
and bare names host-wide would force a root-owned `/etc/hosts`. After `up`, publishing records is
rootless (user-owned hostsdir) and the fabric GC pins the fabric while host-route is on - so
create/destroy republish names with **no further sudo**; only `up`/`down` touch sudo. `down` reverts
the resolver (`resolvectl revert`) and removes the veth. (Suffix default `cs.sandbox`, override
`CS_SANDBOX_DNS_SUFFIX`.)

## Implementation notes

**`cs-sandbox` integration.** `cmd_create` branches - the Podman path is unchanged; the Firecracker
path:

- builds/uses the cached artifacts and the per-sandbox disks;
- allocates a subnet address + SSH port (microVMs draw 2300-2399);
- writes `run.json` (boot-source + drives + vsock + a virtio-net tap);
- launches Firecracker via `fc_launch` (fabric up → tap → host→VM forwarder → `podman unshare
  --rootless-netns` firecracker into the netns), recording `fcip`/`port`/`cpus`/`mem`/repoclones in
  `instances/<name>/meta`. A VM that never signals `FC-VM-READY` is torn down and the create fails loudly.

Lifecycle: `start` re-asserts the name registration and relaunches (after `e2fsck -p`); `stop` shuts
the VM down (in-guest sync+reboot, then kill) and GCs the fabric; `rm`/`destroy` also drop the tap, the
name registration, and the disks. `exec` / `claude-login` / `codex-login` go over `ssh` (no `podman
exec` equivalent).

**Concurrency.** Parallel creates race on shared host state (IP/port allocation, the one-per-host
fabric, image builds). A host-wide lock (`instances/.create.lock`) wraps only the race-sensitive
prefix (allocate → write the meta claim → fabric up); the long parts (disk builds, the boot wait) run
unlocked so creates overlap. The claim is written before the long build, and an EXIT trap reaps a
failed create so it can't leak its reserved address/port. The cache builds are concurrency-safe
(PID-private temp + atomic `mv`).

**Engine specifics.** `fakeroot` fakes ownership but not *read* permission, so the `0000`
`shadow`/`gshadow` are made readable before `mke2fs`. Firecracker drives **virtio-over-MMIO**, so
`virtio_mmio` is in the initramfs.

## Constraints

Firecracker is a deliberately lean rust-vmm/KVM VMM, which trades features for a small surface:

- **No `virtio-fs`, so no live host-directory share.** Directory sharing is a RO ext4 disk
  (`--snapshot`) or the alternates clone (`--repo`) - never a live `--mount`.
- **Shared objects are point-in-time.** Refresh the disk, or `fetch` for later host commits; reflink
  makes the rootfs copy ~free on btrfs/xfs.
- **VM names are sandbox-registered** on create/destroy rather than auto-discovered the way container
  names are - but `ssh <other>` is identical.
- **Linux/KVM only** - macOS always uses the Podman engine.
