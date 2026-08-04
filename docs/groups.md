# Groups

**Groups are opt-in.** Without `--group`, every sandbox joins one called `default` and behaves
exactly as the rest of the docs describe: one network, every sandbox reachable by name. Nothing here
is something you have to know to use `cs-sandbox`. Read on when two experiments — or any two
efforts — share your host and must not interfere.

A **group** is an isolation boundary for a set of sandboxes: its own network, its own SSH trust
material, and its own gateway. Members of a group reach each other by name; members of different
groups neither resolve nor connect to one another — and could not authenticate even if some future
change made them reachable.

The motivating case is running the *same fixture twice*: two experiments comparing approaches, each
needing its own copy of the same sandboxes, concurrently and without contaminating each other.

```bash
cs-sandbox group create cache-redis
cs-sandbox create api   --group cache-redis --type agent --repo ~/projects/api
cs-sandbox create bench --group cache-redis --type agent --repo ~/projects/bench
```

`group create` is optional — `create --group X` brings the group up if it does not exist.
`CS_SANDBOX_GROUP` sets the default for new sandboxes; without it, sandboxes join `default`.

## Identity is (group, name)

A sandbox is identified by its group *and* its name, so the same name may exist in several groups.
That is what lets a harness use one fixed naming scheme (`api`, `bench`, …) for every experiment it
runs, instead of inventing unique names and threading them through everything downstream.

The canonical reference is **`<name>.<group>`**, and addressing follows one rule with no
exceptions: **a bare name means the default group.** A member of any other group is only ever
reachable as `<name>.<group>`.

```bash
cs-sandbox exec api.cache-redis ls        # a member of cache-redis
cs-sandbox exec api                       # the api in the DEFAULT group, and nothing else
```

A bare name is never resolved against "whichever group happens to hold it uniquely". That would
make a reference's meaning depend on the rest of the host: `ssh api` works today, then some
unrelated experiment creates its own `api` and the same command either breaks or — far worse —
keeps working while denoting a different sandbox. A reference that always means the same thing is
worth more than one that saves typing. When a bare name misses, the error names the qualified
references that do exist, so the fix is obvious.

`ls -q` and `ls --json` both emit the qualified reference, because that is the field you feed back
into another command. `--json` is the stable machine-readable inventory:

```json
{ "ref": "api.cache-redis", "name": "api", "group": "cache-redis",
  "status": "running", "engine": "podman", "network": "cs-sandbox-cache-redis" }
```

`group ls -q` and `group ls --json` are the same two forms one level up. `-q` prints the group
names alone, one per line, so a group listing pipes as readily as a sandbox one:

```bash
cs-sandbox group ls -q | xargs -n1 cs-sandbox group rm -f
```

`--json` is the inventory of groups themselves, for a tool that creates and reclaims them and needs
to check its work without parsing a table:

```json
{ "name": "cache-redis", "network": "cs-sandbox-cache-redis",
  "gateway": 2401, "members": 2, "created": "2026-01-01T00:00:00Z" }
```

It answers on an empty host with `[]`, so "no groups" is distinguishable from "this build has no
such command".

## What a group owns

| Artifact | Name | Purpose |
|---|---|---|
| Podman network | `cs-sandbox-<group>` | the isolation boundary (`isolate=true`) |
| SSH keys | `keys/groups/<group>/` | trust material, valid only inside the group |
| Gateway | `cs-sandbox-<group>-keepalive` | pins the bridge; published as the group's ssh jump host |
| Fabric dir | `net/<group>/` | per-group dnsmasq state and VM name records |
| Tap prefix | recorded in `group.json` | allocated, not hashed — see below |

The group record lives at `<instances>/<group>/group.json`, with each member's record beside it at
`<instances>/<group>/<name>/state.json`. Podman object names (container, volumes) carry the group
as `<name>.<group>` because they are host-global; the guest hostname and the in-network DNS alias
stay bare, so members keep reaching each other as plain `<name>`.

A `--repo` clone's branch carries the group too, because the host source repo it is fetched back
into sits outside every group: `cs-sandbox/<name>.<group>`, against `cs-sandbox/<name>` for the
default group. See [repo-sharing.md → Branches and groups](repo-sharing.md#branches-and-groups).

`group rm` refuses while members exist; `-f` destroys them first. Removing a group reclaims its
network, gateway, keys and [host-route leg](#host-route-and-groups). The default group's network is
shared host-wide and is never reclaimed.

## Two layers of isolation

**The network.** Every group network is created `--opt isolate=true`, the default group's included.
Separate bridges and subnets are *not* sufficient on their own: netavark otherwise forwards traffic
between bridges in the same rootless namespace. `isolate=true` blocks that without the loss of
outbound internet that `--internal` would cause. A pre-existing network of the same name is **not**
adopted unless it inspects as ours and isolated — silently adopting a stranger's bridge would void
the boundary.

Isolation is **not** one-sided: measured on netavark, `isolate=true` on one network does not block
a *non-isolated* peer. Both networks have to carry it, which is why the default fabric cannot
simply be left as it was. `isolate` is a creation-time option, so a default network created before
this existed is recreated on first use — but only when nothing except its own keepalive is
attached, otherwise create fails with the list of sandboxes to clear first. A named group's network
is never recreated: an unlabelled network bearing that name may be the user's own.

**The keys.** Each group has its own user and agent keys. This is the layer that makes the design
robust rather than merely correct: if `isolate` ever regresses — a netavark upgrade, a hand-edited
network — a single shared key would turn a reachability bug into an immediate breach. With
per-group keys it stays a reachability bug.

Measured on Podman 5.8.2 / netavark, two groups on one host:

| | Result |
|---|---|
| Cross-group by raw IP | blocked |
| Outbound internet from a member | works |
| Group A's key against a group B sandbox | `Permission denied (publickey)` |
| Group B's own key against a group B sandbox | succeeds |

Tap prefixes are **allocated and recorded**, not derived from a hash of the group name. Linux
interface names are host-global, so a hash collision between two groups would surface as an
interface-name clash a long way from its cause; allocation makes it impossible rather than
improbable. Two microVMs, one per group, both land on `.200` of their own subnet and get
`fd0000200` and `fd0001200` — same octet, different interface.

## Reaching a group from the host

Host access does **not** use the sandbox network at all. Each sandbox's sshd is published on
`127.0.0.1:<port>`, and `sync-ssh-config` writes a block per member:

```
Host api.cache-redis
    HostName 127.0.0.1
    Port 2203
    IdentityFile .../keys/groups/cache-redis/id_cs-sandbox_user
```

The bare alias is emitted for default-group members only — the same rule the CLI applies, so `ssh`
and `cs-sandbox` can never disagree about which sandbox a name denotes. This plane keeps working
when a group's fabric is broken, which is exactly when you need to get in.

**The gateway** is the second route, and the one that gives you names. Each group publishes one
port fronting its keepalive container, which is also its ssh jump host:

```bash
ssh cache-redis-gw                                 # a shell inside the group
ssh -L 8080:api:8000 cache-redis-gw                # reach a member's service BY NAME
```

Inside the group, names resolve over the group's own DNS — the same path members use to reach each
other — so one published port reaches every member and any port on them. Note that `ssh -J` to a
member's *alias* will not work: that alias maps to the member's published loopback port, which
means nothing inside the group. Address members by their bare in-group name through the gateway.

The gateway authorizes only its group's key, and its ssh config block deliberately offers only
that key: presenting the host's own identities first would exhaust sshd's `MaxAuthTries` before
the right key was ever tried.

## host-route and groups

`host-route` gives the host name-based access to sandboxes over `.cs.sandbox`, for any protocol
rather than ssh alone. It works by putting one end of a veth pair on the host and enslaving the
other to a fabric bridge, so the host becomes an L2 participant on that network.

Groups are separate bridges on separate subnets, so one veth reaches exactly one of them:
host-route wires **one leg per group**, and names follow the same rule as every other reference —
bare for the default group, qualified everywhere else.

```bash
cs-sandbox host-route up
curl http://api.cs.sandbox:8000            # a default-group member
curl http://api.cache-redis.cs.sandbox:8000 # cache-redis's own api, distinctly
```

Each group keeps its **own** fabric dnsmasq, serving only its own names on its own subnet, and gets
its own systemd-resolved scope (`~<group>.cs.sandbox` → that group's DNS). Publishing every group's
inventory into one shared resolver would hand each group the names of all the others, which is the
boundary groups exist to draw. Resolved routes each query to the most specific matching domain, so
the group scopes and the default `~cs.sandbox` coexist without either knowing about the other.

Interface names come from the group's allocated tap prefix with `fd` swapped for `hr`
(`fd0001` → `hr0001h`/`hr0001n`). The swap is not cosmetic: the fabric identifies running microVMs
by scanning the namespace for interfaces starting with the tap prefix, so a leg sharing it would be
counted as a VM — pinning the fabric against GC and feeding a non-numeric octet into VM address
allocation.

Wiring a leg needs root, so a group created *after* `host-route up` is not reachable until one more
`cs-sandbox host-route refresh`. `host-route status` reads the wiring back from the host rather
than reporting what it intended, and names any group whose leg is missing or stale.

Unwiring one does not need root: deleting either end of a veth destroys the pair, and the namespace
end belongs to you — so `group rm` retires its own leg without sudo. It has to. netavark removes a
network's bridge only once nothing is left attached to it, so a leg outliving its group pins the
bridge, and that bridge keeps the address of the subnet it was built for. Podman names interfaces by
scanning its own networks, never the namespace, so it hands `podmanN` to the next network it creates
and netavark adopts the interface as it finds it — leaving that group's members with a gateway that
does not exist. No DNS, no outbound, and nothing anywhere that says why. `create` therefore also
evicts a bridge already squatting the name of a network it has just made.

**The host is not a router between groups.** A host holding legs on two group bridges could
forward between them — netavark's isolation lives inside the rootless namespace, and the host's own
forwarding path is separate. Sandboxes hold `NET_ADMIN`, so a member can add a route to another
group's subnet via the host's leg on its own subnet, which is what a compromised agent would do.

host-route closes that by disabling forwarding **on each leg**, written before the link is brought
up in the same transaction that creates it:

```
/proc/sys/net/ipv4/conf/<leg>/forwarding = 0
/proc/sys/net/ipv6/conf/<leg>/forwarding = 0
```

`ip_forward()` gates on the **incoming** interface, so a leg with this off carries no transit at
all — not to another group, and not to the host's own LAN either. Host access is untouched, because
traffic to and from the host is INPUT and OUTPUT and never reaches the forwarding path.

This is deliberately not a firewall rule. It is a core IPv4 knob, present wherever host-route can
run at all, whereas nftables availability, module coverage and set-type support vary by distribution
and kernel build — WSL2 being the obvious case. It also needs no cleanup: the knob disappears with
the interface. A `DROP` rule between leg pairs would additionally have missed leg→LAN transit.

**How it drifts, and how that surfaces.** Writing the *global* `net.ipv4.ip_forward` propagates to
every interface, so a later `sysctl -w net.ipv4.ip_forward=1` — or a Docker install doing it at
package time — silently re-enables forwarding on legs wired earlier. `up` and `refresh` therefore
re-assert the knob on every leg rather than only on ones they create, and `status` reads the knobs
back from `/proc` (no privilege needed) and reports **DEGRADED**, never plain `UP`, while any leg
forwards.

**Measured, with a control.** Two groups on one host, `ip_forward=1`, routes added on both sides,
and both legs placed in firewalld's `trusted` zone (target ACCEPT) so the firewall could not be the
thing doing the work:

| A→B through the host's leg | TCP | ICMP |
|---|---|---|
| `forwarding=1` (positive control) | REACHABLE | REACHABLE |
| `forwarding=0` (as host-route sets it) | BLOCKED | BLOCKED |

The control is the point: it shows the bypass is real rather than hypothetical, so the blocked row
means the knob closed it rather than that the probe never worked. The blocked row used the value
`host-route refresh` sets itself, not a hand-written one.

Before this existed, A→B was already blocked on this host — but by firewalld's
`filter_FORWARD_POLICIES` catch-all reject, which belongs to one host's firewall configuration and
would not hold on a host without one. That is why the boundary is enforced here rather than assumed.

**The gateway remains the stronger option**: one published port per group, name-based access inside
it, and no host-level path between groups at all — no dependency on host firewall state.
