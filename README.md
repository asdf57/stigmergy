# stigmergy

Homelab API attempt deux

## Run the stack

The API, etcd, and OpenBao run together with Docker Compose:

```sh
make run
```

This builds the API image and starts both datastores before the API. API data is
retained in `etcd-data`; encrypted OpenBao data is retained in `openbao-data`.
OpenBao listens only on host loopback at <http://127.0.0.1:8200>.

For a detached stack, use `make up`. Follow logs with `make logs` and stop the
stack with `make down`.

To include the development-only etcd browser, run:

```sh
make up-tools
```

Open <http://127.0.0.1:8002>, add an etcd v3 connection, and enter
`etcd:2379` in its server field. Do not use `127.0.0.1`: from inside the
workbench container that address refers to the workbench itself. The browser
is bound to host loopback and is not started by the normal `make run` or
`make up` commands. It can modify and delete stored keys, so treat it as an
administrative debugging tool.

Once running:

- API readiness: <http://127.0.0.1:8080/readyz>
- OpenAPI document: <http://127.0.0.1:8080/openapi.json>
- Swagger UI: <http://127.0.0.1:8080/docs/>

Docker publishes the API on `0.0.0.0:8080`, so it is also reachable through
the host's LAN addresses. The etcd client port remains restricted to host
loopback.

`make run-local` remains available when intentionally running the API process
on the host against an independently managed etcd endpoint.

### Automatic OpenBao bootstrap

`make run` is the only command required for the local stack. The Compose
bootstrap service initializes OpenBao on its first run, unseals it on every
subsequent run, enables KV v2 and AppRole, installs the least-privilege
`stigmergy-api` policy, and provisions the Agent credentials before the API
starts.

OpenBao uses single-node integrated Raft storage rather than dev mode. Its
unseal key and initial root token are retained with mode `0700`/`0600` in the
dedicated `openbao-init` Docker volume, which is mounted only by the short-lived
bootstrap container. The Agent receives only RoleID and SecretID through the
separate `openbao-bootstrap` volume. It exchanges those for a renewable token
in `openbao-runtime`, which the API mounts read-only. The API never receives the
unseal key, root token, RoleID, or SecretID.

This automated credential retention is intentionally a local-development
tradeoff. The OpenBao listener is plain HTTP and bound only to host loopback.
A production deployment needs TLS, multiple unseal or recovery shares, and an
external auto-unseal and bootstrap identity mechanism. The `openbao-data` and
`openbao-init` volumes form one state set: deleting only one makes the retained
OpenBao data unrecoverable or its saved credentials stale.

## Machine reports

The typed MachineReport API accepts consumable hardware observations. The
machine controller derives an LLDP switch/port location, finds or creates the
durable Machine for that location, projects the observation into its status,
then deletes the processed report using optimistic concurrency:

- `POST /api/v1alpha1/machine-reports` creates a report resource.
- `GET /api/v1alpha1/machine-reports` lists reports awaiting consumption.
- `GET /api/v1alpha1/machine-reports/{name}` fetches a pending report.
- `PUT /api/v1alpha1/machine-reports/{name}` idempotently creates or replaces
  a pending report, making it the preferred endpoint for periodic agents.
- `DELETE /api/v1alpha1/machine-reports` deletes every report and returns the
  number deleted.
- `DELETE /api/v1alpha1/machine-reports/{name}` deletes a report using its
  current ETag in `If-Match`.

The OpenAPI request validator rejects missing required fields, unknown object
properties, invalid enums, out-of-range values, and malformed date-times before
the generated handler is called.

`Machine` and `Server` intentionally have different lifecycles:

- `Machine` is the controller-created physical asset at an LLDP location.
  `Machine.spec.location` is its stable attachment identity and
  `Machine.status.inventory` contains observed hardware.
- `Server` is the user-named desired host. `Server.spec.machineSelector.location`
  selects a Machine and the remainder of the spec contains hostname, operating
  system, users, packages, sysctls, feature flags, and provisioning controls.
- The binding controller records UID-qualified `Server.status.machineRef` and
  `Machine.status.serverRef` values. A Server without a matching Machine remains
  Pending; a Server cannot steal a Machine already bound to another Server.

For example, save this as `desktop.yaml` after the report controller has
discovered the physical Machine (or before—it will remain Pending until then):

```yaml
apiVersion: homelab.io/v1alpha1
kind: Server
metadata:
  name: desktop
  labels:
    homelab.io/role: workstation
spec:
  machineSelector:
    location:
      lldp_port: bridge/ether3
      switch_mac: d4:01:c3:27:91:67
  hostName: desktop
  domainName: homelab.local
  networking:
    management:
      interfaceSelector:
        attachedAtMachineLocation: true
      addressSelector:
        family: ipv4
        subnet: 10.1.1.0/24
  packages:
    - curl
    - vim
  provisioning:
    enabled: true
  reconciliation:
    paused: false
```

Upload it with:

```sh
curl --fail-with-body \
  -H 'Content-Type: application/yaml' \
  --data-binary @desktop.yaml \
  http://127.0.0.1:8080/api/v1alpha1/servers
```

The Server controller resolves the management interface by matching the
Machine's LLDP switch/port location back to the reporting host interface, then
selects exactly one address from the configured subnet. It publishes the result
under `Server.status.networking.management`. Zero or multiple matching
addresses produce a `ManagementAddressReady=False` condition; the controller
does not guess between NICs or virtual functions.

## Dynamic Ansible inventory

`InventoryCaptureGroup` selects manageable resources across registered kinds by
metadata labels. The inventory controller materializes an Ansible-ready group
from each selected resource's resolved management address:

```yaml
apiVersion: homelab.io/v1alpha1
kind: InventoryCaptureGroup
metadata:
  name: servers
spec:
  selector:
    matchLabels:
      homelab.io/managed-by: ansible
    matchExpressions:
      - key: homelab.io/environment
        operator: In
        values: [lab]
```

Omitting `matchKinds` selects across every manageable kind. To select every
Server regardless of labels while remaining precise after kinds such as
`Router` are added, use:

```yaml
spec:
  selector:
    matchKinds:
      - apiVersion: homelab.io/v1alpha1
        kind: Server
```

```sh
curl --fail-with-body \
  -H 'Content-Type: application/yaml' \
  --data-binary @servers.yaml \
  http://127.0.0.1:8080/api/v1alpha1/inventory-capture-groups
```

The resulting status is shaped for direct conversion to Ansible inventory:

```yaml
status:
  phase: Ready
  inventory:
    servers:
      hosts:
        desktop:
          ansible_host: 10.1.1.251
          fqdn: desktop.homelab.local
  matchedResources: 1
  capturedResources: 1
  omittedResources: []
```

The controller considers registered durable resource kinds generically. A
selected object is capturable when it exposes the small management contract:
`status.networking.management.address.address` plus optional `status.fqdn`.
Future resources such as `Router` require no inventory-specific schema marker
or inventory-controller change.

Selected resources without a resolved management address are not guessed or
silently included. They appear in `status.omittedResources`, and the group
phase is `Partial` or `Pending` until ready. `matchLabels` and Kubernetes-style
`In`, `NotIn`, `Exists`, and `DoesNotExist` match expressions are supported.
Changes to any registered inventory-source kind automatically trigger
reconciliation.

Labels such as `homelab.io/managed-by: ansible` are user-owned selection intent.
Resource controllers do not add or restore them automatically.

### Publishing inventory to Git

An `InventoryPublication` connects a capture group to a destination resource.
For a Git destination, define the repository separately so its URL, branch,
credential source, and commit identity can be reused:

```yaml
apiVersion: homelab.io/v1alpha1
kind: GitRepository
metadata:
  name: ansible-inventory
spec:
  url: https://github.com/example/homelab-inventory.git
  branch: main
  authentication:
    username: x-access-token
    passwordEnvironmentVariable: GITHUB_TOKEN
  commit:
    authorName: Stigmergy
    authorEmail: stigmergy@homelab.local
    messageTemplate: "Update {{ .Publication.metadata.name }}"
---
apiVersion: homelab.io/v1alpha1
kind: InventoryPublication
metadata:
  name: servers-to-git
spec:
  inventoryCaptureGroupRef:
    name: servers
  format: ansible-yaml
  destinationRef:
    apiVersion: homelab.io/v1alpha1
    kind: GitRepository
    name: ansible-inventory
  path: inventories/homelab/servers.yaml
  policy:
    mode: OnChange
    requireReady: true
```

Store the objects as `git-repository.yaml` and `inventory-publication.yaml`;
the API intentionally accepts one YAML document per request. For example:

```sh
curl --fail-with-body -H 'Content-Type: application/yaml' \
  --data-binary @git-repository.yaml \
  http://127.0.0.1:8080/api/v1alpha1/git-repositories

curl --fail-with-body -H 'Content-Type: application/yaml' \
  --data-binary @inventory-publication.yaml \
  http://127.0.0.1:8080/api/v1alpha1/inventory-publications
```

The controller uses a pure-Go Git client. It reads the token from the process
environment variable named by `passwordEnvironmentVariable`; the secret is
never stored in either resource. For Docker Compose, export the token before
starting the stack:

```sh
export GITHUB_TOKEN='github-token-value'
make up
```

The publication controller waits for a Ready capture group by default, renders
the inventory deterministically, and commits only when the destination file's
content changes. Its status records the inventory digest, repository UID and
generation, resulting Git revision, and success or failure conditions. Pushes
are non-forced. A missing credential or rejected push leaves the publication in
`Failed` without changing the repository.

## Server-scoped SSH access

`SSHAccessGrant` associates one login user with one Server. Its controller
generates an Ed25519 key pair, stores both halves in OpenBao with create-only
semantics, and publishes only the public key and fingerprint through API
status. Neither private keys nor OpenBao tokens are persisted in etcd.

First describe the OpenBao backend in `openbao-secret-store.yaml`:

```yaml
apiVersion: homelab.io/v1alpha1
kind: SecretStore
metadata:
  name: openbao
spec:
  provider:
    openBao:
      address: http://openbao:8200
      kvV2Mount: kv2
      keyPrefix: secrets
      authentication:
        tokenFile: /run/openbao/token
```

Then create `desktop-matt-ssh.yaml`:

```yaml
apiVersion: homelab.io/v1alpha1
kind: SSHAccessGrant
metadata:
  name: desktop-matt
spec:
  serverRef:
    name: desktop
  loginUser: matt
  credential:
    generatedKeyPair:
      algorithm: ed25519
      keyName: matt
      secretStoreRef:
        name: openbao
```

Submit both resources:

```sh
curl --fail-with-body -H 'Content-Type: application/yaml' \
  --data-binary @openbao-secret-store.yaml \
  http://127.0.0.1:8080/api/v1alpha1/secret-stores

curl --fail-with-body -H 'Content-Type: application/yaml' \
  --data-binary @desktop-matt-ssh.yaml \
  http://127.0.0.1:8080/api/v1alpha1/ssh-access-grants
```

For these names, the controller derives the logical KV path
`secrets/desktop/ssh-keys/matt` beneath the `kv2` mount. `metadata.name` remains
globally unique while `keyName` controls only the leaf inside that Server's
path. The stored value
contains `privateKey`, `publicKey`, `serverUID`, and `accessGrantUID`. Existing
material is adopted only when both immutable UIDs match; otherwise the grant
reports `SecretOwnershipConflict` and the controller never overwrites it.

A Ready grant is projected into the Server API contract that homelabd consumes:

```yaml
status:
  ssh:
    authorizedKeys:
      - accessGrantRef:
          name: desktop-matt
          uid: ...
        loginUser: matt
        publicKey: ssh-ed25519 AAAA...
        fingerprint: SHA256:...
```

homelabd talks only to this API and installs the resolved public keys. It never
connects to OpenBao. Every grant receives the
`homelab.io/ssh-access-cleanup` finalizer. Deleting a grant first removes its
public Server projection, verifies the OpenBao record's Server and grant UIDs,
permanently deletes that KV v2 record, and then completes API deletion. If
OpenBao is unavailable or ownership does not match, the grant remains visible
with `metadata.deletionTimestamp` until cleanup can safely succeed.

Machine and Server specs support two update styles:

- `PUT /api/v1alpha1/machines/{name}` replaces the complete spec.
- `PATCH /api/v1alpha1/machines/{name}` applies an RFC 7396 JSON Merge Patch,
  requires the current ETag in `If-Match`, and replaces arrays atomically.

Use the corresponding `/api/v1alpha1/servers/{name}` paths to modify desired
host configuration. Server updates never rewrite Machine inventory or manage
SSH key-pair storage.

Resource creation and replacement also accept `application/yaml` and
`application/x-yaml`. Merge patches may use `application/merge-patch+yaml`.
YAML is normalized to JSON before the same OpenAPI validation and handlers run;
multi-document request bodies are rejected.

## API contract and resource modules

[`internal/api/spec/openapi.yaml`](internal/api/spec/openapi.yaml) contains the
small shared API contract: system endpoints and common metadata/error schemas.
Concrete resources are self-contained modules under
`internal/api/spec/resources`.

```text
internal/api/
├── spec/          authored API contract and generated bundle
│   └── resources/ one authored file per resource type
├── gen/           generated OpenAPI Go models and embedded document
├── registry/      resource registration runtime and generated entries
├── cmd/           API generation tooling
├── server.go      server construction
├── routing.go     registered-resource dispatch
├── handlers.go    shared CRUD/upsert handlers
├── conversion.go  typed/storage conversion and stored-data validation
└── middleware.go  validation errors, limits, timeouts, and recovery
```

Each module declares its registration and all schemas unique to that resource:

```yaml
x-stigmergy-resource:
  api-version: homelab.io/v1alpha1
  path-prefix: /api/v1alpha1
  kind: MachineReport
  plural: machine-reports
  spec-schema: MachineReportSpec
  operations: [create, list, get, put, delete, delete-collection]

MachineReportSpec:
  type: object
  additionalProperties: false
  # ...
```

The project generator discovers every `resources/*.yaml` module and synthesizes
its concrete REST paths, create/resource/list envelopes, operation metadata,
and Go resource registration. It writes the bundled document under `spec/`,
the generated type models under `gen/`, and the generated registration under
`registry/`.

The public API remains fully typed, but all registered resources share the same
internal CRUD/upsert handlers. Adding another standard resource only requires
adding its resource module and regenerating; the base OpenAPI and runtime API
packages do not need editing.

Regenerate the API boundary after changing the contract:

```sh
make generate
```

Do not edit `internal/api/spec/openapi.bundle.yaml`,
`internal/api/registry/registry.gen.go`, or
`internal/api/gen/openapi.gen.go` by hand. When the server is running, the
bundled OpenAPI document is available at `/openapi.json` and Swagger UI at
`/docs/`.
