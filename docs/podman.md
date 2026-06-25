# sandbox - Podman container engine

`cs-sandbox --engine podman` - the **default on macOS**, and available on any host - runs each
sandbox as a rootless **Podman container** instead of a Firecracker microVM, reusing the same OCI
image, the same `cs-sandbox` CLI, and the same SSH + repo capabilities. This document covers what is
specific to the container engine; the cross-engine model (trust, the generic image, networking,
shared image stores, agent tooling) lives in [`design.md`](design.md).

## Why a container

The container shares the host kernel - no separate guest kernel to build or boot - so it is the
**lighter, faster-to-start** engine, the **only** engine on macOS / non-KVM hosts, and the one that
supports a **live `--mount`** share (a real host-directory bind-mount, which a microVM has no
`virtio-fs` to offer). The trade-off: isolation rests on the **container boundary** (a scaled-down
capability set + seccomp, bounded by your unprivileged host user) rather than a separate kernel -
which is why the autonomous **agent** type defaults to the microVM on a Linux/KVM host. See
[firecracker.md](firecracker.md#why-a-microvm) for the other side of that trade.

The one prerequisite is **Podman** itself (and, on macOS, `podman machine init && podman machine
start`); `./cs-sandbox doctor --engine podman` checks it. See [`INSTALL.md`](../INSTALL.md).

## Container boot

The image bakes in **no** developer identity (the cross-engine reasons are in
[design.md](design.md#anatomy-of-a-sandbox)); the container path creates your user at first boot.

`cs-sandbox` launches the container **`--userns=keep-id --user 0:0`** and passes your identity + the
sandbox config as environment (`CS_SANDBOX_USER`/`UID`/`GID`/`HOME`, plus `CS_SANDBOX_TYPE` / `YOLO`
/ `SSH_PORT` / `IMAGE_STORES`), so PID1 - the **entrypoint** - runs as container-root, which keep-id
maps to your host uid. The entrypoint then:

1. creates the matching group + user and grants it NOPASSWD sudo;
2. adds the subuid/subgid range for nested Podman;
3. seeds and chowns the home, installs the seed's SSH material + agent creds, and starts sshd;
4. `runuser`s to that user for the main process.

Because keep-id maps the created uid to your host uid, ownership stays correct on both sides. The
per-sandbox **seed** that step 3 reads (`authorized_keys`, the tier key, `ssh_config`, agent creds…)
is shared with the microVM engine and described in
[design.md](design.md#the-per-sandbox-seed).

### Home volume

The home is a **named volume** `cs-sandbox-home-<name>` mounted at `/home/<user>` - correct Linux
permissions on both OSes (which sshd's strict checks require), with no virtiofs perm issues. The
trade-off: no direct host `cd` into the home - use `cs-sandbox exec`, `ssh`, or `podman cp`. It
persists across stop/start; `cs-sandbox destroy` removes it.

## Nested Podman

In a **microVM** you are real root with your own kernel, so `podman` just works natively and the
rest of this section does not apply. In a **Podman container**, true isolated podman-in-podman
needs two things: a **scaled-down capability set** on the outer container, and a **rootful** inner
engine.

**Scaled-down caps, still rootless.** The container runs *rootless* - engine and container bounded
by your unprivileged host user via `--userns=keep-id` - granting only what the inner rootful
Podman needs:

- `CAP_SYS_ADMIN` - nested userns + mounts
- `CAP_NET_ADMIN` - inner netavark bridge
- `CAP_MKNOD` - inner device nodes
- `CAP_SYS_PTRACE` - pasta opening the build worker's netns during `podman build`
- `/dev/net/tun` + `--security-opt unmask=/proc/sys` - so inner netavark can write `net.ipv4.ip_forward`

The default seccomp filter stays **on** and `/proc/kcore` + host devices stay masked. Because the
container is rootless the caps are namespaced - bounded by your host user, with no host-root path
absent a kernel bug - so this is strictly safer than `--privileged`, which turns seccomp off and
unmasks everything. `--privileged` (or `CS_SANDBOX_NESTED_CAPS=privileged`) is a one-flag fallback
if a kernel/podman version regresses the scaled-down set.

**Rootful inner engine.** Nested Podman runs *rootful inside the container* (container-root, which
under keep-id is your unprivileged host user). Plain `podman` is a `/usr/local/bin/podman` wrapper
(`exec sudo /usr/bin/podman`, ahead of `/usr/bin` on PATH; no setuid; reuses the user's NOPASSWD
sudo), so shells, scripts, and `ssh <name> podman …` all hit the rootful engine. The vendored
**`user-podman`** builds on it: for `run`/`create` it injects `--user UID:GID` plus matching
`--passwd-entry`/`--group-entry`, so the inner container runs as your uid:gid and its bind-mount
files come back owned by you rather than by a subuid.

Why both are required: a *rootless* inner Podman needs `newuidmap`/`newgidmap`, but `--userns=keep-id`
leaves no cleanly sub-dividable subuid range and the image drops `newuidmap`'s file caps - so
rootless-inner fails *even with `--privileged`*. A *rootful* inner Podman runs as container-root
and, with the namespaced `CAP_SYS_ADMIN`, sets up the nested userns/mounts directly. So the working
combination is `CAP_SYS_ADMIN` + rootful-inner, with native overlay and seccomp still on.

The image carries `crun`, `slirp4netns`, `passt`, a `storage.conf` defaulting to **native**
`overlay` (no `mount_program`; `fuse-overlayfs` is a fallback only), and `containers.conf` with
`cgroups = "disabled"` - which silences a benign cgroup-v2 warning on every nested run (a rootless
nested container can't delegate controllers to its children). The only cost is that resource
limits don't apply to nested containers, which they couldn't anyway in this setup.

SELinux confinement is turned off for the container (`--security-opt label=disable`); no `:Z`/`:z`
relabeling is applied, which also avoids the macOS virtiofs-relabel problem.

### Per-sandbox container storage

A dedicated volume `cs-sandbox-containers-<name>` mounts at the rootful store
`/var/lib/containers`, so nested images/layers persist across recreation, don't bloat the home
volume, and sit on a **non-overlay** backing filesystem - which lets the kernel's native overlay
work instead of falling back to slower `fuse-overlayfs`. On first boot the entrypoint probes
`podman info`; if native overlay isn't usable it writes a `fuse-overlayfs` fallback into the
system `storage.conf` and caches the decision. The two drivers share an on-disk format, so
switching is non-destructive.

Sharing images *across* sandboxes (rather than re-pulling per sandbox) is the cross-engine
**shared image stores** feature - see [design.md](design.md#shared-image-stores).

## macOS

macOS is first-class - the Podman engine runs inside the **podman-machine** VM (Firecracker is
Linux/KVM-only, so macOS always uses Podman). Everything behaves as on Linux:

| Capability | Linux | macOS | Notes |
|---|:---:|:---:|---|
| host → sandbox SSH by name | ✓ | ✓ | gvproxy forwards `127.0.0.1:<PORT>` into the VM |
| sandbox → sandbox SSH by name | ✓ | ✓ | VM-internal aardvark-dns |
| trust matrix (H/U/G) | ✓ | ✓ | VM-internal |
| named-volume home + seed | ✓ | ✓ | correct perms on both |
| directory sharing | ✓ | ✓* | *shared paths must be under a machine-shared root (under `$HOME`) |
| nested Podman | ✓ | ⚠️ | works inside the VM; inner images must match the VM's architecture |

(`host-route` is Linux-only - see
[design.md](design.md#optional-reach-sandboxes-directly-by-name-host-route).)
