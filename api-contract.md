# API Resource Contract

This document defines the shared meaning of Stigmergy API resources. It records
both the behavior implemented today and the semantic rules expected of every
resource kind.

## Resource shape

```text
Object
├── type identity
│   ├── apiVersion
│   └── kind
├── metadata
│   ├── name
│   ├── uid
│   ├── resourceVersion
│   ├── generation
│   ├── creationTimestamp
│   ├── labels
│   └── annotations
├── spec
└── status
```

`spec` contains desired or declared state. `status` contains state observed by
controllers. Metadata identifies an object and supports lifecycle and
concurrency behavior shared by all resource kinds.

## Field ownership and meaning

| Field | Set by | Meaning |
| --- | --- | --- |
| `apiVersion` | Registry or caller | Schema version used to interpret the object. |
| `kind` | Registry or caller | Resource type. |
| `metadata.name` | Client or controller | Stable DNS-style name within a resource kind. |
| `metadata.uid` | Store on create | Immutable identity for one lifetime of an object. Recreating the same name produces a new UID. |
| `metadata.resourceVersion` | Store from the etcd revision | Opaque optimistic-concurrency token. Clients must not interpret it as object generation. |
| `metadata.generation` | Store | Starts at `1` and increments when `spec` changes. Metadata-only changes do not increment it. |
| `metadata.creationTimestamp` | Store on create | Immutable server creation time in UTC. |
| `metadata.deletionTimestamp` | Store on delete | Time graceful deletion was requested. The resource remains addressable while finalizers are present. |
| `metadata.labels` | Client or controller | Organizational metadata used for selection and grouping. Controllers add labels only when their resource contract explicitly says so. |
| `metadata.annotations` | Client or controller | Opaque metadata. An annotation has no behavior unless a component explicitly defines and documents it. |
| `metadata.finalizers` | API defaults and controllers | Ordered cleanup barriers. Clients cannot set them during creation. Removing the last finalizer from a terminating resource completes deletion. |
| `spec` | Client or controller | Desired or declared state for the resource kind. |
| `status` | Controller | Observed state, replaced through the controller-only optimistic status update operation. |

etcd is the persistence and transaction mechanism; it does not define the
object contract. The store assigns server-owned metadata and derives
`resourceVersion` from etcd revisions.

## Semantic rules

1. `apiVersion` and `kind` select the resource schema. `kind` and
   `metadata.name` locate the object in the current storage keyspace, while
   `metadata.uid` distinguishes separate lifetimes that reuse the same name.
2. Server-owned fields must not be supplied during creation and must remain
   immutable except when changed by the store according to this contract.
3. Resource definitions are authored in OpenAPI YAML. Controllers use the
   generated concrete resource types and their typed specs. A resource's
   generated `Encode` method converts it to the generic storage representation.
4. Controllers must reconcile idempotently. Reprocessing the same input must
   converge without repeatedly creating or updating unchanged resources.
5. Controllers may modify only fields they own. Ownership conventions for a
   resource kind must be documented alongside that kind.
6. Annotations are informational unless their behavior is explicitly
   documented. An annotation must not be treated as an owner reference,
   finalizer, or other lifecycle mechanism by implication.
7. `generation` represents desired-state changes. `resourceVersion` represents
   any persisted write and is used for concurrency checks.
8. Features must not be exposed as meaningful contract behavior before their
   lifecycle is implemented. Garbage collection and owner references remain
   unimplemented.

## Current lifecycle

- Create assigns a UID, generation `1`, creation timestamp, and resource
  version.
- PUT creates or replaces the complete client-owned desired state: `spec`,
  labels, and annotations. Its manifest identity must match the resource URL.
  It preserves server/controller-owned metadata and status, increments
  generation only when `spec` changes, and skips storage writes when desired
  state is unchanged. `If-Match` makes replacement conditional; without it,
  the API retries bounded conflicts caused only by server/controller-owned
  updates and rejects concurrent changes to client-owned state.
- Merge patch requires the expected resource revision, recursively merges JSON
  objects in `spec`, replaces arrays atomically, and validates the complete
  merged spec before updating it.
- Status update requires the expected resource revision, replaces only status,
  and preserves metadata, spec, and generation.
- Delete requires the expected resource revision. Resources without finalizers
  are removed immediately. Resources with finalizers receive a deletion
  timestamp and remain visible until a controller removes the last finalizer.
- Collection deletion applies the same graceful-deletion behavior to each
  resource instead of bypassing finalizers.
- Recreating a deleted name creates a distinct object with a new UID.
- Controller startup lists and enqueues the current resources for every watched
  kind, then watches from the following store revision. Existing Pending or
  terminating resources therefore resume reconciliation after a restart.

## Machine and Server ownership

- `MachineReport` is a transient hardware observation. Its controller consumes
  it after projecting the latest inventory into a Machine.
- `Machine.spec.location` identifies the physical attachment point;
  `Machine.status.inventory` is observed hardware and
  `Machine.status.serverRef` is the active binding.
- `Server.spec.machineSelector` and the remaining Server spec express the
  user's desired host placement and configuration.
- The binding controller exclusively owns `Server.status.machineRef`,
  `Machine.status.serverRef`, and the `MachineBound` Server condition. Binding
  references contain both name and UID.
- When requested by `Server.spec.networking.management`, the Server controller
  resolves the local interface attached at `Machine.spec.location` from LLDP
  inventory and publishes exactly one subnet-matching address under
  `Server.status.networking.management`. Ambiguous results remain unresolved.
- Binding is exclusive and eventually consistent. An unmatched Server remains
  Pending and a Server targeting an already-bound Machine reports Conflict.
- `InventoryCaptureGroup.spec.selector` selects a resource universe across
  registered durable kinds. Optional `groups` selectors evaluate only that
  universe and can assign one host to multiple Ansible groups. `groupVars.all`
  applies to every captured host; other keys must name declared groups.
  Selection supports `matchKinds`, `matchLabels`, and the standard set-based
  label operators. Capture capability is determined from canonical
  management-address status, not from a schema marker or controller-added
  label. The resource's metadata name never becomes an Ansible group.
  The inventory controller captures only resources with a resolved canonical
  management address. The capture-group name is the Ansible group name and the
  selected resource's `metadata.name` is the host name.
- `GitRepository.spec` describes a reusable Git destination and names the
  process environment variable containing its credential. Secret material is
  never copied into resource spec or status. Its branch is the base used when
  a publication's target branch does not exist yet.
- `InventoryPublication.spec` connects one capture group to a destination and
  exclusively owns `target.rootPath` on `target.branch`. Its controller creates
  a missing target branch from the repository's configured branch and
  deterministically renders an Ansible directory containing the configured
  inventory filename and `group_vars/<group>.yaml` files, removes stale owned
  artifacts, and publishes the complete change in one Git commit. Status records
  source and destination generations, the artifact-set digest, individual artifact paths and digests,
  and the resulting Git revision.
- Publication requires a Ready capture group unless explicitly disabled. Git
  updates are ordinary non-forced commits to the target branch.
- `SecretStore.spec` describes how controllers reach an external secret store;
  authentication names either a process environment variable or an absolute
  Agent-managed token file and never contains the credential value itself.
- `SSHAccessGrant.spec` associates one login user with one Server and requests a
  generated key pair. The SSH access controller creates and owns a same-named
  `Secret`, waits for its observed generation to become Ready, and checks the
  stored Server and grant UIDs before adopting an existing Secret.
- The Secret controller, rather than the SSH access controller, writes and
  removes SSH key material in the configured external store. The grant status and
  `Server.status.ssh.authorizedKeys` contain the public key and fingerprint for
  homelabd. The Server controller does not create or store SSH credentials.
- `SSHAccessGrant` receives `homelab.io/ssh-access-cleanup` at creation.
  Deleting it immediately removes its public Server projection and requests
  deletion of its owned Secret. The Secret finalizer removes the external
  value; the grant finalizer is removed only after the Secret is gone.

## Reserved future capabilities

The following concepts are intentionally not part of the implemented contract
yet:

- owner references and garbage collection;
- a shared cross-resource condition framework;
- field ownership or managed fields;
- namespaces and resource scope;
- admission, defaulting, or mutation hooks.

Each capability should be added here only when its behavior, validation, and
tests are implemented.
