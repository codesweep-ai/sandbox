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
**`cs-sandbox/<name>`** — or **`cs-sandbox/<name>.<group>`** outside the default group, see
[Branches and groups](#branches-and-groups). (On macOS the source must be under `$HOME`, as for the
other modes.)

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

The result is a writable tree on `cs-sandbox/<name>` borrowing history read-only, with new commits
going to the sandbox's own tiny object store.

**Git identity carries over**, so commits are attributed correctly. Each clone's *local*
`user.name`/`user.email` is set to whatever identity that repo uses on the host — resolving a local
override, an `includeIf`, or the global — and the sandbox's *global* `~/.gitconfig` is seeded from
the host's global one. Both are captured at create time, and the global is set only if unset, so a
later in-sandbox change is never clobbered.

Re-runs on later boots are no-ops: Podman guards on `~/.cs-sandbox-repos-done`, Firecracker skips
any `~/<dir>` that already has a `.git`.

## Delivering the source objects (the one engine-specific part)

Both engines expose the source's objects read-only and re-attach them at a **stable path within the
sandbox** - alternates stores an absolute path, which must be identical on every boot of *that*
sandbox (it need not match across engines):

- **Podman:** `-v <hostrepo>:/run/cs-sandbox-repos/<dir>:ro` - zero copy, reading the host's live
  objects.
- **Firecracker:** a read-only ext4 disk holding `git clone --bare <repo>` — one point-in-time
  object copy — attached at `/run/cs-sandbox-repo-<n>`. The disk is content-addressed and cached, so
  VMs off the same commit share one build and one disk can attach RO to many at once. Device order
  and cache mechanics are in [`firecracker.md`](firecracker.md#disks).

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
branch), so a sandbox created before a naming change keeps the branch it was created with.

`cs-sandbox inspect <name>` prints that record, and `--json` makes it machine-readable (abridged
here — the object also carries `name`, `type`, `engine`, `network`, `created`, `port`, the
firecracker `ip`/`cpus`/`mem`, `agentlogins`, `snapshots` and `imagestores`):

```json
{ "ref": "api.cache-redis", "group": "cache-redis", "status": "running",
  "repos": [ { "dir": "app", "source": "/src/app", "branch": "cs-sandbox/api.cache-redis" } ] }
```

Read the branch from there rather than composing it. The rule below is stable, but a caller that
reimplements it agrees only until it changes — and then computes a plausible wrong answer, which is
harder to notice than not being able to ask.

### Branches and groups

The host source repository is not inside any group — it is the one place two groups meet. So the
branch carries the group everywhere except the default one:

| Sandbox | Branch |
|---|---|
| `api` (default group) | `cs-sandbox/api` |
| `api.cache-redis` | `cs-sandbox/api.cache-redis` |

Without this, the case groups exist for — running the same fixture twice, each with its own copy of
the same sandboxes — breaks on the way home: both copies' `api` target
`refs/heads/cs-sandbox/api`, and the second `fetch` is rejected as a non-fast-forward.

The group is appended rather than nested (`cs-sandbox/<group>/<name>`) because nesting puts a
directory where a ref may already be: a default-group `api` owns `refs/heads/cs-sandbox/api`, so a
*group* named `api` could not create `refs/heads/cs-sandbox/api/<member>` — git rejects it with
`cannot lock ref`. Appended, the two are siblings. It also means the branch is spelled exactly like
the sandbox reference you would pass to any other command.

The default group keeps the bare `cs-sandbox/<name>`: it is what every example here shows, what
existing host repos already contain, and with one group there is nothing to disambiguate.

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
  the instance. Recreating with the **same name** reuses that home, so the checkout and its commits
  come back; pass the **same `--repo`** too, so the read-only source the clone borrows re-attaches.
  `ls` keeps listing the data with status **`removed`** until you reuse or delete it.
- **destroy** drops the home (volume / `rootfs.ext4`), so the sandbox's commits are gone - **`fetch`
  before `destroy`** if it has unmerged work. It also works on a name whose sandbox `rm` already
  removed, which is how you reclaim data you decided not to keep after all.
- **Don't `git gc --prune` the source** while a Podman sandbox has it borrowed (the source is
  bind-mounted read-only into the live container). A Firecracker sandbox is immune - its disk is a
  point-in-time copy.

## Implementation

- The spec parser (`internal/spec`) strips `:NAME` first (a slash-free, non-empty tail), then
  `@REF`, derives `dir` from the **resolved** path, checks each is a git repo, and rejects duplicate
  names. `--snapshot` shares the grammar minus `@REF`.
- One engine hook is the only divergence: Podman adds `-v …:ro`, Firecracker builds and attaches the
  cached RO disk (`internal/fcdisk`); the first-boot clone and `fetch`/`push` are engine-agnostic.
  The seed `repos` manifest is **6 fields on Podman** (`dir`, RO-source path, branch, base,
  `user.name`, `user.email`) and **5 on Firecracker**, where the disk's mount point is positional.
- The sandbox's typed state (`internal/state`, persisted to
  `instances/<group>/<name>/state.json`) records one `repoclone` entry per repo — source, dir, and
  branch — for `fetch`/`push`.

This engine-independence is what lets the microVM engine share repos without `virtio-fs`.
