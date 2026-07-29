# sandbox - repo sharing (`--repo`, fetch/push)

`--repo` shares a git repo into a sandbox as an **isolated, lightweight, per-sandbox checkout**
that works the same on both engines (Podman containers and Firecracker microVMs). This document
covers `--repo`; the plain directory mode (`--snapshot`) is in
[`design.md`](design.md#directory-sharing).

## The two sharing modes, at a glance

| flag | model | mutability | engines | use when |
|---|---|---|---|---|
| `--snapshot PATH[:NAME]` | **frozen copy** (Podman `cp`+`:ro`; Firecracker RO ext4) | RO, point-in-time | both | freeze inputs/data at create time |
| `--repo PATH[@REF][:NAME]` | **per-sandbox clone borrowing the source's git objects** | RW on its own branch | both | isolated git work / agents; portable; retrieve via git |

`--snapshot` takes **any** directory; `--repo` requires a git repo (a worktree or a
bare repo). All are repeatable and land at `~/<name>` (default name = basename of the **resolved**
path - `--repo .` → the current directory's real name; `:NAME` overrides). For `--repo`, `@REF`
sets the base commit (default: the source's `HEAD`), and the checkout lives on branch
**`cs-sandbox/<name>`**. (On macOS the source must be under `$HOME`, as for the other modes.)

## How it works (engine-agnostic)

The checkout is a `git clone --shared` off a **read-only copy of the source**: the clone keeps its
own refs, index, working tree, and new objects, and reads existing history read-only from the
source via `objects/info/alternates`. So it is fully writable, copies **no** history (a KB-sized
clone), and never touches the source.

> **Why not `git worktree`?** A worktree writes metadata back into the source's `.git` (refs,
> `worktrees/`), so it fails against a read-only source. Alternates are the purpose-built "borrow
> objects read-only, write everything else locally" mechanism.

On **first boot only**, the guest init reads the seed `repos` manifest (one line per `--repo`) and,
as the developer user, runs for each entry:

```bash
git clone --shared <ro-source> ~/<dir>                         # <ro-source> = the RO objects mount (below)
git -C ~/<dir> config receive.denyCurrentBranch updateInstead  # let host `push` update the work tree
git -C ~/<dir> switch -c cs-sandbox/<name> <base> \
  || git -C ~/<dir> switch -c cs-sandbox/<name>                # branch at @REF; falls back to HEAD if @REF won't resolve
```

The result is a writable tree on `cs-sandbox/<name>` borrowing history read-only; new commits go to
the sandbox's own (tiny) object store. **Git identity carries over** so commits are attributed
correctly: each clone's *local* `user.name`/`user.email` is set to the identity that repo uses on
the host (`git -C <repo> config user.*`, which resolves a local override, `includeIf`, or the
global), and the sandbox's *global* `~/.gitconfig` is seeded from the host's global `user.name`/
`user.email` (both captured at create time; the global is only set if unset, so a later in-sandbox
change is never clobbered). Re-runs on later boots are no-ops: Podman guards on
`~/.cs-sandbox-repos-done`; Firecracker skips any `~/<dir>` that already has a `.git`.

## Delivering the source objects (the one engine-specific part)

Both engines expose the source's objects read-only and re-attach them at a **stable path within the
sandbox** - alternates stores an absolute path, which must be identical on every boot of *that*
sandbox (it need not match across engines):

- **Podman:** `-v <hostrepo>:/run/cs-sandbox-repos/<dir>:ro` - zero copy, reading the host's live
  objects.
- **Firecracker:** a read-only ext4 disk holding `git clone --bare <repo>` (one point-in-time object
  copy), attached at `/run/cs-sandbox-repo-<n>`. The repo disks are `vdc…`, in the order `cs-sandbox`
  appended them (repos, then `--snapshot`, then `--image-store`), which the guest walks with a single
  device-letter cursor. The disk is **content-addressed cached** (key = `sha256` of the source's ref
  tips + HEAD, 40 hex; file `<srcid>-<key>.ext4`), so VMs from the same commit reuse one build
  (`cp --reflink=auto` per sandbox) and one disk can attach RO to many VMs at once. Cached disks
  unused for `CS_SANDBOX_FC_REPO_CACHE_TTL_DAYS` (default 14) are pruned. See
  [`firecracker.md`](firecracker.md).

## Retrieve / update - host-initiated, works for agents

All git transport runs from the **host** over the host→sandbox SSH alias (`<name>`), so it works
even for agent sandboxes that can't SSH back. Both directions are **fast-forward-only** (no `+` in
the refspec; git's `updateInstead` default) - a diverged branch is rejected with a hint.

- **`cs-sandbox fetch <name> [dir]`** - `git -C <hostsource> fetch <name>:<dir>
  cs-sandbox/<name>:refs/heads/cs-sandbox/<name>`. Only the sandbox's **new** commits transfer (the
  host already has the base); each `--repo`'s work lands on a `cs-sandbox/<name>` branch in its own
  source repo (no cross-repo collision).
- **`cs-sandbox push <name> [dir]`** - `git -C <hostsource> push <name>:<dir>
  HEAD:cs-sandbox/<name>`. Sends host-side commits into the sandbox;
  `receive.denyCurrentBranch=updateInstead` (set on the clone at create time) updates the sandbox's
  working tree - rejected unless the tree is clean and the push fast-forwards.

`[dir]` selects one repo when a sandbox has several. `fetch`/`push` read the host source repo and
branch from the sandbox's state record (one `repoclone` entry per repo: source, dir, and the
`cs-sandbox/<name>` branch).

## Peer-to-peer - fetch/push between sandboxes

The host commands above are the usual path, but a **user** sandbox can also fetch or push a peer
**agent** sandbox's branch directly - the same operation, initiated from inside a sandbox instead of
the host. It works because a user sandbox reaches agent sandboxes by name over SSH (every sandbox
authorizes the user-tier key), and both clones borrow the same base objects, so only the new commits
transfer. It's **user→agent only**: agents can't SSH back into a user sandbox. Nothing here uses the
`cs-sandbox` CLI (it isn't installed inside sandboxes) - it's plain git over the SSH the fabric
already provides.

From inside the user sandbox, in its own clone (`~/<dir>`):

```bash
# Fetch a peer agent's branch. `<agent>:<dir>` is scp-style host:path — <dir> is the repo dir
# relative to the peer's home (~/<dir>), and <agent> resolves by name on the fabric.
git fetch worker:api cs-sandbox/worker      # worker = agent sandbox, api = repo dir
git log --oneline FETCH_HEAD                 # the agent's commits; merge/cherry-pick as usual
# …or keep it as a local branch:
git fetch worker:api cs-sandbox/worker:cs-sandbox/worker
git switch cs-sandbox/worker
```

Push the other way, into the peer's checkout - same fast-forward + clean-tree rules as `cs-sandbox
push` (the clone's `receive.denyCurrentBranch=updateInstead` updates its work tree):

```bash
git push worker:api HEAD:cs-sandbox/worker
```

## Lifecycle & safety

- **stop/start** keeps the running sandbox and all its disks - `start` resumes the same instance
  with its data.
- **rm keeps the data** (the home volume on Podman, `rootfs.ext4` on Firecracker) and removes only
  the instance itself. Recreating with the **same name** reuses that home, so the checkout and its
  commits come back; pass the **same `--repo`** so the read-only source re-attaches (the clone borrows
  its objects). The commits live in the home, so they survive the `rm`. `ls` keeps listing that data
  with status **`removed`** until you reuse or delete it.
- **destroy** drops the home (volume / `rootfs.ext4`), so the sandbox's commits are gone - **`fetch`
  before `destroy`** if it has unmerged work. It also works on a name whose sandbox `rm` already
  removed, which is how you reclaim data you decided not to keep after all.
- **Don't `git gc --prune` the source** while a Podman sandbox has it borrowed (the source is
  bind-mounted read-only into the live container). A Firecracker sandbox is immune - its disk is a
  point-in-time copy.

## Implementation

- The spec parser (`internal/spec`) parses `--repo` specs - strips `:NAME` first (a slash-free,
  non-empty tail), then `@REF`; derives `dir` from the **resolved** path; validates each is a git repo
  (`.git` or `objects/`); rejects duplicate names. It mirrors `--snapshot`'s PATH[:NAME] grammar (the
  same directory-spec parsing in `internal/spec`).
- One engine hook is the only divergence: Podman adds `-v …:ro`; Firecracker builds + attaches the
  cached RO disk (the content-addressed repo-disk cache in `internal/fcdisk`). The first-boot clone and
  `fetch`/`push` are engine-agnostic. The seed `repos` manifest is **6 fields on Podman** (`dir`, the
  RO-source path, branch, base, `user.name`, `user.email`) and **5 on Firecracker** (`dir`, branch,
  base, `user.name`, `user.email`) - the disk's mount point is positional.
- The sandbox's typed state (`internal/state`, persisted to `instances/<name>/state.json`) records
  one `repoclone` entry per repo — source, dir, and branch — for `fetch`/`push`.

This engine-independence is what lets the microVM engine share repos without `virtio-fs`.
