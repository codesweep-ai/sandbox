package lend

// The lender on the fabric.
//
// A lender used to be a host process on a fixed port, and that is what made two
// people on one machine collide: 2500 is one port, bound wide because a sandbox
// reaches a host process at the host's ordinary address (SPEC R152). The second
// caller's `create` found something answering there, adopted it, and reported a
// loan the other user's lender would never honour.
//
// It is a container on the group's own network now. Sandboxes reach it by name
// over that network, nothing is bound on the host, and two runs on one machine
// cannot reach each other's lender because their networks cannot: the container
// has no interface on any network but its own, so the boundary is a namespace
// rather than a rule about who may call.
//
// What does NOT change is the trust model. The credential still lives in the
// host's filesystem and is read per request (R150); the mount is a view of the
// same file, not a copy. The binary is the same one the CLI runs, because it is
// mounted from the host — see Spec.Bin.

// GuestName is what a sandbox calls the lender. It is a network alias on the
// group's network, so it resolves for a container through aardvark and for a
// microVM through the fabric's dnsmasq, which forwards everything that is not a
// VM name to aardvark.
//
// Reserved: a sandbox may not take this name, or its own alias would answer for
// the lender on the network they share. state.ValidName refuses it.
const GuestName = "cs-lender"

// BoxName is the lender container for one network. Named after the network the
// way the keepalive is, because that is its lifetime: one lender per group,
// serving the sandboxes that can reach it.
func BoxName(network string) string { return network + "-lender" }
