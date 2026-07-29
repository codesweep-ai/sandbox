# sandbox - Firecracker microVM engine

`cs-sandbox --engine firecracker` - the **default** engine on a Linux/KVM host - runs each sandbox
as a Firecracker **microVM** instead of a Podman container, reusing the same OCI image, the same
`cs-sandbox` CLI, and the same SSH + repo capabilities. This document covers what is specific to the
microVM engine; the cross-engine model (trust, the generic image, agent tools) lives in
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

The cost: with no `virtio-fs`, the rootfs and shared directories are delivered to a microVM as
block devices — ext4 disks built on the host and attached to the guest.

## Prerequisites

All rootless - no host `sudo` to *run* it - but the engine shells out to host packages, which
`cs-sandbox` preflight-checks (failing with an actionable install line). `cs-sandbox build`
downloads the SHA256-verified Firecracker binary and builds the guest kernel + base rootfs into the
artifact cache (`$XDG_CACHE_HOME/cs-sandbox`, i.e. `~/.cache/cs-sandbox`); `create` then just boots
from it. **`cs-sandbox doctor` (which defaults to `--engine firecracker`) checks all of the below and
prints the exact fix for anything missing.**

- **Packages:** `passt` (Podman's rootless uplink, which VMs share), `dnsmasq` (the forwarding
  VM-name resolver), `fakeroot` + `e2fsprogs` (build the ext4 disks), `socat` + `python3` (the
  host→VM port/vsock bridges), `shadow-utils`/`uidmap` (`newuidmap`, for Podman's rootless userns),
  `iproute`/`iproute2` (`ip`, for the tap/bridge fabric), `openssh-clients`/`openssh-client` (`ssh`,
  to reach the booted VM), `curl`, `git`. The preflight detects `dnf` vs `apt` and prints the right
  names:

  ```bash
  # Fedora
  sudo dnf install podman passt dnsmasq fakeroot e2fsprogs socat python3 shadow-utils iproute openssh-clients curl git
  # Ubuntu / Debian  (uidmap not shadow-utils, dnsmasq-base not dnsmasq, iproute2 not iproute)
  sudo apt install podman passt dnsmasq-base fakeroot e2fsprogs socat python3 uidmap iproute2 openssh-client curl git
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

> The base **Podman** install (and, on macOS, a sized podman machine — see
> [`INSTALL.md`](../INSTALL.md)) is the one prerequisite shared with the Podman engine - it builds
> the image and provides the network fabric on every host. The Firecracker engine is Linux/KVM-only; on macOS / non-KVM hosts sandboxes
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
- **`CS_SANDBOX_FC_KERNEL=host`:** boot the running host kernel (`uname -r`) instead of the pinned
  fedora one - smaller and auto-tracking host upgrades, but **Fedora-host only** (it relies on the
  host's `dracut` and a readable `/boot/vmlinuz-<ver>`). Note that `cs-sandbox build` does **not**
  build host-mode artifacts: it reuses a previously-cached `vmlinux.elf`/`initrd.img` set if one is
  present, otherwise it errors and points you back to the default fedora kernel.

The cached artifacts are `vmlinux.elf` + `initrd.img` + `modules.tar` (+ `kver`) under the artifact
cache (`~/.cache/cs-sandbox`).

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

1. the seed builder (`internal/seed`, shared with the Podman engine) writes `instances/<name>/seed/`:
   `authorized_keys`, the tier key, stable `host_keys/`, the sandbox-scoped `ssh_config`, `host_hosts`
   (reach the host by name), and `claude/` + `codex/` credentials (including the API-key/cloud `env` +
   `creds/` when present).
2. the firecracker engine (`internal/engine/firecracker.go`) copies those into an `fc-seed/` dir and
   adds **`cs-sandbox.conf`** - the identity + network contract: `CS_SANDBOX_USER`/`UID`/`GID`/`HOSTNAME`
   (the bare sandbox name), `TYPE`, `YOLO`, `IP`, `GW`, `DNS` - plus the separate `repos` /
   `snapshots` / `imagestores` manifest files.

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
6. **first boot:** seed `/home/<user>` from the image skeleton (`/sandbox/home`); **every boot:**
   refresh the managed ssh material (authorized_keys, tier key, `ssh_config`
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
  forwards everything else to aardvark. It is found by scanning for a live dnsmasq on *our* address:
  a resolver already serving *our* hostsdir is adopted rather than duplicated, and one holding the
  address while serving a *different* hostsdir is reported as a conflict by name instead of being
  left to fail with a bare "Address already in use". Asking the host this way means a root that never
  started the resolver still sees it — and no pidfile can go stale underneath it.

**Name resolution across engines.** VM → anything: the VM's resolver is the dnsmasq (VM names local,
the rest forwarded to aardvark). Container → VM: Podman pins a container's `resolv.conf` to aardvark
but records its `--dns` servers and forwards misses to them - so `cs-sandbox create` passes
`--dns <dnsmasq>` and aardvark forwards an unknown VM name to our dnsmasq, which answers it.

**Outbound** is Podman's own pasta uplink - VMs NAT out like containers. The fabric comes up lazily
on the first sandbox and is reclaimed only when no VM runs **and** no `cs-sandbox` container besides
the keepalive remains **and** host-route is off.

**The fabric is host-global; an instances dir is not.** There is one netns, one bridge and one
loopback per host, but `CS_SANDBOX_HOME` / `CS_SANDBOX_INSTANCES_DIR` can point at several
independent sandbox roots (a second checkout, a test run). A root reads only *its own* `state.json`
files, so that list is never a complete picture of what is on the fabric. Anything a second root
could otherwise collide with is therefore decided by asking the host:

- **VM address** - a candidate in `<prefix>.200-.250` is taken if this root records it *or* the tap
  `fdt<lastoctet>` already exists in the netns. The taps are the authoritative record: they are
  host-global and outlive whichever root created them.
- **Host SSH port** - a candidate is taken if this root records it *or* something already answers on
  `127.0.0.1:<port>`. A stopped sandbox is caught by the first check (nothing is listening for it),
  another root's running forwarder only by the second.
- **Fabric GC** - a live tap counts as "a VM still needs this", the same as a locally recorded
  running VM.
- **`~/.ssh/config.d`** - one managed fragment **per root** rather than one shared file, so
  regenerating one root's `Host` blocks cannot delete another's (see
  [design.md](design.md#networking-and-name-resolution)).
- **Stale name records** - a create killed outright (no deferred cleanup) leaves its name in the
  hostsdir. Fabric bring-up sweeps records whose address has no tap: names are published only once
  the tap exists, so "no tap" means nothing answers there. The test is host-global, which is what
  makes it safe here - a sweep driven by one root's instance list would delete the live names of
  every sandbox it cannot see. A stopped sandbox loses its record and re-registers on `start`.

The fabric's own working dir (the dnsmasq hostsdir + log, the host-route marker) is host-global for
the same reason: `$XDG_CACHE_HOME/cs-sandbox/net`, independent of `CS_SANDBOX_HOME` and
`CS_SANDBOX_FC_CACHE`, so every root shares one. (`CS_SANDBOX_FC_NET` overrides it for isolated
runs; diverging it while a fabric is up is what the conflict check above reports.)

### host → VM ssh

The host can't address the rootless netns directly, so `ssh <name>` reaches the guest via a published
host port bridged with a **unix socket** (sockets ignore network namespaces): a host-side `socat`
binds the port (the managed `~/.ssh/config.d` fragment gives the name `HostName 127.0.0.1` / `Port N`
/ `HostKeyAlias`, written the same way for both engines)
and relays through `fwd.sock` to a per-VM `socat` inside the netns that connects to the guest's `:22`.
Per-VM and lifecycle-tracked.

A Firecracker **vsock** is retained as a no-IP standby transport (it is *not* the routine ssh path):
guest CID 3; PID1 is `socat VSOCK-LISTEN:22 → TCP4:127.0.0.1:22`; the host side is a hybrid-vsock unix
socket `instances/<name>/vm.vsock`, and `image/guest/vsock-connect` speaks Firecracker's `CONNECT <port>` →
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

**`cs-sandbox` integration.** `cs-sandbox create` dispatches by engine - the Podman path (the
`internal/engine` podman adapter) is unchanged; the Firecracker path (the firecracker engine,
`internal/engine/firecracker.go`):

- builds/uses the cached artifacts and the per-sandbox disks (`internal/fcdisk`);
- allocates a subnet address + SSH port (microVMs draw 2300-2399, via `internal/ports`), skipping
  anything already live on the host as well as anything recorded (see "The fabric is host-global");
- writes `run.json` (boot-source + drives + vsock + a virtio-net tap);
- launches Firecracker via the engine's `launch` step (fabric up via `internal/fcnet` → tap → host→VM
  forwarder → `podman unshare --rootless-netns` firecracker into the netns), recording
  `fcip`/`port`/`cpus`/`mem`/repoclones in the sandbox's typed state (`internal/state`, persisted to
  `instances/<name>/state.json`). A VM that never signals `FC-VM-READY` is torn down and the create fails loudly.

The name is registered in `launch`, immediately after the tap comes up, so a record and a link share
a lifetime (what makes the stale-record sweep above sound).

Lifecycle: `start` relaunches, re-asserting the name; `stop` shuts
the VM down (in-guest sync+reboot, then kill) and GCs the fabric; `rm`/`destroy` also drop the tap, the
name registration, and the disks. `exec` / `agent-login` go over `ssh` (no `podman
exec` equivalent).

**Concurrency.** Parallel creates race on shared host state (IP/port allocation, the one-per-host
fabric, image builds). The lock is per-root, so it serializes creates within one instances dir; the
host-level checks above are what keep *different* roots from colliding. A host-wide lock (`internal/lock`, `instances/.create.lock`) wraps only the
race-sensitive prefix (allocate → write the state claim → fabric up); the long parts (disk builds, the
boot wait) run unlocked so creates overlap. The claim is written before the long build, and an EXIT trap reaps a
failed create so it can't leak its reserved address/port. The cache builds are concurrency-safe
(PID-private temp + atomic `mv`).

**Engine specifics.** `fakeroot` fakes ownership but not *read* permission, so the `0000`
`shadow`/`gshadow` are made readable before `mke2fs`. Firecracker drives **virtio-over-MMIO**, so
`virtio_mmio` is in the initramfs.

## Constraints

Firecracker is a deliberately lean rust-vmm/KVM VMM, which trades features for a small surface:

- **Directory sharing is a block device (no `virtio-fs`).** A shared directory comes in as a RO
  ext4 disk (`--snapshot`) or the alternates clone (`--repo`).
- **Shared objects are point-in-time.** Refresh the disk, or `fetch` for later host commits; reflink
  makes the rootfs copy ~free on btrfs/xfs.
- **VM names are sandbox-registered** on create/destroy rather than auto-discovered the way container
  names are - but `ssh <other>` is identical.
- **Linux/KVM only** - macOS always uses the Podman engine.
