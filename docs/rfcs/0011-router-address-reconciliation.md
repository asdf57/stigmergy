# RFC 0011: Owned RouterOS address reconciliation

Status: Implemented locally; not deployed or accepted against a physical router.

## Contract

Router.spec.addresses is an optional list of named IPv4 interface addresses.
Each entry has name, address (host CIDR), network and interface. The API exposes
a named RouterAddress type. Names are unique DNS-style identifiers; host IPs
must also be unique. Network must match the masked CIDR except for RouterOS /32
point-to-point addressing, which may identify a different peer network.
actual-interface, flags and RouterOS IDs are observations, not desired settings.
No DHCP pools, MetalLB resources, DNS, firewall or VLAN configuration is added.

Omitting addresses leaves the entire section unmanaged. An explicit empty list
requests removal of eligible owned entries only. Router deletion does not remove
live addresses. Keep management migrations deliberate and separate.

## Ownership and existing configuration

New entries carry the exact comment
`stigmergy:router:<Router UID>:address:<entry name>`. This is the stable identity;
address/interface/network are settings, not identity. Updates use RouterOS set
on the existing ID instead of delete/recreate. Comments are reserved for ownership
on created entries; the controller does not overwrite unrelated human comments.

A static, enabled, valid, unowned entry that exactly matches address, network and
configured interface satisfies desired state without mutation or adoption. This
allows existing router configuration to be declared without a network migration.
Removing that declaration later does not remove the unowned entry. Changing its
settings causes a visible conflict, not takeover. Another Router's ownership,
dynamic addresses, conflicting host IPs and ambiguous duplicate entries cannot be
silently adopted. Explicit adoption, if needed, is a separate reviewed manual step
that marks the exact entry after inspecting its identity and network settings.

Missing desired entries are created. Only entries carrying this Router UID's
ownership prefix and absent from desired state are candidates for removal.
Unrelated, foreign-owned and dynamic entries are preserved. Owned dynamic entries
cannot satisfy a desired static entry and are never deleted by cleanup.

## Safety and execution

The reconciler resolves Ready credentials, constructs a client for the actual
management endpoint and performs an authenticated probe. Client commands receive
the current context and have bounded connection/command timeouts; credentials and
contexts are not left in an uninitialized client stored on the reconciler.

A pure planner validates the entire desired/observed plan before writes. It
refuses creating, updating or removing any address prefix that contains the
management API IP. Existing exact management matches may remain declared without
writes. This conservatively protects broad prefixes such as the site's /8 as well
as the management /32. Malformed owned addresses cannot be removed automatically.
These protections are not a proof that arbitrary router changes preserve every
route, client connection or external dependency; deployment still requires review.

Additions and in-place updates precede removal of obsolete owned entries. Before
each write the reconciler rechecks Router UID/generation/deletion and relists
RouterOS state, validating ownership and conflicts again. A changed plan stops
execution instead of applying a stale ID. Failures stop subsequent writes; there
is no blind rollback, forced cleanup or management-address repair. Successfully
completed earlier changes converge idempotently on a later pass.

A final read verifies the desired state and owned cleanup before reporting Ready
with AddressesReconciled. Conflicts, rejected mutations and failed verification
report Failed with a specific condition; a successful probe alone is not success
when addresses are managed. Generic status revision checks prevent old results
from being written over changed desired resources.

## Verification and rollout

Local fake-router tests cover the site's existing /8 and /32 layout, omitted and
empty sections, unowned/dynamic preservation, ownership collisions, duplicate and
invalid desired inputs, management guards, safe mutation order, read-back failure,
concurrent edits and idempotency. Transport tests cover decoding and cancellation.

Review the full Router manifest and live address table before deploying. Existing
matching entries need no re-creation or comment rewrite. Deploy generated API and
controller together, then inspect Router conditions and actual RouterOS settings.
Do not run physical-router mutation tests, change management routing, clear an
Operation or restart the platform as a substitute for scoped acceptance.
