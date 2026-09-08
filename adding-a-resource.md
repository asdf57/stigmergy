# Adding a New Resource Type

This guide describes the complete process for adding any resource type to
Stigmergy. It uses a hypothetical `Secret` resource as a running example, but
the same steps apply to `Network`, `Volume`, `Certificate`, or any other kind.

Use these substitutions throughout the guide:

| Concept | Generic form | Example |
| --- | --- | --- |
| Go/OpenAPI kind | `<Kind>` | `Secret` |
| URL plural | `<plural>` | `secrets` |
| Schema file | `<resource>.yaml` | `secret.yaml` |
| Spec schema | `<Kind>Spec` | `SecretSpec` |
| Status schema | `<Kind>Status` | `SecretStatus` |
| Controller package | `<resource>` | `secret` |

## What is generated and what is hand-written

The resource module is the source of truth for the API boundary. From it, the
generator creates:

- the resource's REST paths and request/response envelopes;
- OpenAPI Go models such as `apigen.SecretSpec`;
- a typed controller-facing resource, constructor, encoder, and decoder;
- the runtime registry entry used by the generic API handlers; and
- the bundled OpenAPI document served by `/openapi.json` and `/docs/`.

The generator does **not** create the resource's reconciliation behavior. A
controller, dependency watches, external-service clients, and their tests are
hand-written because those parts express the resource's domain semantics.

Adding a passive resource that only needs validated CRUD can stop after API
generation and tests. Add a controller only when the resource must converge
status, manage another resource, or manage external state.

## 1. Define the resource contract

Before writing its schema, decide:

- what belongs in client-owned `spec`;
- what belongs in controller-owned `status`;
- which other resource kinds it references or reacts to;
- which create, read, update, patch, and delete operations it exposes;
- whether deletion requires cleanup of external or dependent state; and
- how reconciliation remains safe when it is repeated.

Record resource-specific ownership and lifecycle rules in
[`api-contract.md`](api-contract.md). The shared contract already defines
metadata, generation, resource versions, status ownership, and finalizer
behavior; a new resource should follow those rules instead of redefining them.

For the example, suppose `Secret` declares that a secret value should exist in
an external `SecretStore`. Its spec contains only desired configuration. Its
status records the observed external version, never the secret value itself.

> **Secret-data warning:** the current generic API persists every spec in etcd
> and returns it from GET and LIST. OpenAPI's `writeOnly` keyword alone would
> not change that behavior. Do not add credentials or secret values to a spec
> unless storage encryption, response redaction, logging safety, and access
> control have first been designed and implemented. A resource that points to
> externally stored material is safe with the current boundary.

## 2. Add the resource schema module

Create one authored YAML file under
[`internal/api/spec/resources`](internal/api/spec/resources). For this example,
create `internal/api/spec/resources/secret.yaml`:

```yaml
# Secret declares secret material managed in an external SecretStore.
x-stigmergy-resource:
  api-version: homelab.io/v1alpha1
  path-prefix: /api/v1alpha1
  kind: Secret
  plural: secrets
  spec-schema: SecretSpec
  status-schema: SecretStatus
  operations: [create, list, get, put, patch, delete, delete-collection]
  finalizers: [homelab.io/secret-cleanup]

SecretSpec:
  type: object
  additionalProperties: false
  required: [secretStoreRef, logicalPath, generator]
  properties:
    secretStoreRef:
      $ref: '#/SecretStoreReference'
    logicalPath:
      type: string
      minLength: 1
    generator:
      $ref: '#/SecretGenerator'

SecretStoreReference:
  type: object
  additionalProperties: false
  required: [name]
  properties:
    name:
      type: string
      minLength: 1

SecretGenerator:
  type: object
  additionalProperties: false
  required: [length]
  properties:
    length:
      type: integer
      minimum: 16
      maximum: 4096

SecretStatus:
  type: object
  additionalProperties: false
  properties:
    phase:
      type: string
      enum: [Pending, Ready, Failed]
    observedGeneration:
      type: integer
      format: int64
      minimum: 0
    externalVersion:
      type: integer
      format: int64
      minimum: 1
    conditions:
      type: array
      items:
        $ref: '#/SecretCondition'

SecretCondition:
  type: object
  additionalProperties: false
  required: [type, status, reason]
  properties:
    type: {type: string, minLength: 1}
    status:
      type: string
      enum: ['True', 'False', Unknown]
    reason: {type: string, minLength: 1}
    message: {type: string}
    observedGeneration: {type: integer, format: int64, minimum: 0}
```

Resource metadata has the following meaning:

| Field | Purpose |
| --- | --- |
| `api-version` | Manifest API version and generated type identity. |
| `path-prefix` | API prefix shared by the resource's collection and item paths. It must start, but not end, with `/`. |
| `kind` | Exported Go-style resource name, such as `Secret`. |
| `plural` | Lowercase kebab-case URL segment, such as `ssh-access-grants`. |
| `spec-schema` | Required schema for client-owned desired state. |
| `status-schema` | Optional schema used to validate controller-owned status when resources are read. |
| `operations` | One or more of `create`, `list`, `get`, `put`, `patch`, `delete`, and `delete-collection`. |
| `finalizers` | Optional finalizers assigned by the API on creation. Use one only when deletion must wait for cleanup. |

Every schema name must be an exported Go-style identifier and must be unique in
the combined API. Local references such as `#/SecretGenerator` are rewritten
to component references by the project generator. References to an existing
combined component can use `#/components/schemas/<Schema>`.

Prefer strict object schemas with `additionalProperties: false`, explicitly
list required properties, and add meaningful formats, patterns, bounds, and
enumerations. These constraints are enforced at the HTTP boundary. Keep schema
property names consistent with the API's existing naming conventions because
they become JSON/YAML fields and generated Go fields.

Omit `status-schema` if no controller writes status. Omit `finalizers` if no
cleanup must delay deletion. Expose only the operations the resource contract
actually supports.

## 3. Generate the API boundary

Run the generator from the repository root:

```sh
make generate
```

The command discovers every `internal/api/spec/resources/*.yaml` module. For
`Secret`, it produces symbols including:

- `apigen.SecretSpec` and related schema models;
- `registry.Secret` and `registry.NewSecret`;
- `registry.SecretResource`, whose `Decode` method returns a typed resource;
- a `Secret` entry in `registry.Definitions`; and
- `/api/v1alpha1/secrets` OpenAPI paths and envelopes.

It updates these generated files:

- `internal/api/spec/openapi.bundle.yaml`;
- `internal/api/gen/openapi.gen.go`; and
- `internal/api/registry/registry.gen.go`.

Never edit those files by hand. Change the authored resource module and
regenerate instead. Review the generated diff to catch surprising Go names,
pointer fields, missing operations, or accidental schema changes:

```sh
git diff -- internal/api/spec/openapi.bundle.yaml \
  internal/api/gen/openapi.gen.go \
  internal/api/registry/registry.gen.go
```

At this point the generic router, validation middleware, CRUD handlers, and
etcd store know about the resource automatically. The base OpenAPI file and
generic API packages normally require no edits.

## 4. Implement the controller, if the resource needs one

Create a package under `internal/controller/<resource>`, for example:

```text
internal/controller/secret/
├── controller.go
├── controller_test.go
├── backend.go
└── backend_test.go
```

Keep external systems behind a small interface so reconciliation can be tested
without a live service:

```go
type Backend interface {
    Ensure(context.Context, apigen.SecretSpec, string) (externalVersion int64, err error)
    Delete(context.Context, apigen.SecretSpec, string) error
}
```

The extra string in this example could be the resource UID used for ownership
checks. Define the real interface around the smallest idempotent operations the
controller needs.

A reconciler follows this general shape:

```go
type Reconciler struct {
    store   store.Store
    backend Backend
}

func NewReconciler(resourceStore store.Store, backend Backend) *Reconciler {
    return &Reconciler{store: resourceStore, backend: backend}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
    raw, err := r.store.Get(ctx, registry.SecretResource.Kind, request.Name)
    if errors.Is(err, store.ErrNotFound) {
        return nil // deletion is already complete
    }
    if err != nil {
        return fmt.Errorf("get Secret %q: %w", request.Name, err)
    }

    secret, err := registry.SecretResource.Decode(raw)
    if err != nil {
        return fmt.Errorf("decode Secret %q: %w", request.Name, err)
    }
    if secret.Metadata.DeletionTimestamp != nil {
        return r.finalize(ctx, secret)
    }

    version, err := r.backend.Ensure(ctx, secret.Spec, secret.Metadata.UID)
    if err != nil {
        // Write a useful failure condition when possible, then return according
        // to whether retrying can make progress.
        return fmt.Errorf("ensure Secret %q: %w", secret.Metadata.Name, err)
    }

    status := map[string]any{
        "phase":              "Ready",
        "observedGeneration": secret.Metadata.Generation,
        "externalVersion":    version,
    }
    if resource.EqualJSON(secret.Status, status) {
        return nil // avoid an update/watch/reconcile loop
    }

    revision, err := strconv.ParseInt(secret.Metadata.ResourceVersion, 10, 64)
    if err != nil {
        return fmt.Errorf("parse Secret %q resource version: %w", secret.Metadata.Name, err)
    }
    _, err = r.store.UpdateStatus(ctx, secret.Kind, secret.Metadata.Name, status, revision)
    return err
}
```

The snippet is a shape, not a complete controller. A production reconciler
must also implement the contract-specific error states, ownership checks,
dependency resolution, and deletion behavior.

Follow these invariants in every controller:

1. Treat `store.ErrNotFound` as a normal outcome when the primary resource has
   disappeared.
2. Decode through the generated `registry.<Kind>Resource.Decode` method rather
   than manually casting the generic spec map.
3. Make external operations idempotent. The queue retries returned errors, and
   watch events can cause the same request to run many times.
4. Use `metadata.uid` when proving ownership of external state. A name can be
   deleted and reused by a different resource.
5. Set `status.observedGeneration` only after observing the corresponding spec
   generation.
6. Compare the proposed status with `resource.EqualJSON` before calling
   `UpdateStatus`; otherwise the status write creates another watch event and
   can cause a hot loop.
7. Parse `metadata.resourceVersion` and pass it as the expected revision for
   `Update`, `UpdateStatus`, or `Delete`. A conflict should be retried from a
   fresh read, not overwritten.
8. Put useful state in status, but never put credentials or generated secret
   material there.

### Finalizers and deletion

Use a finalizer only when the controller owns something that must be removed or
detached before the API object disappears. The API assigns configured default
finalizers to newly created resources.

When `metadata.deletionTimestamp` is set, the controller must:

1. verify that the external object belongs to this resource, preferably by UID;
2. perform idempotent cleanup;
3. remove only its own finalizer from the typed resource's metadata;
4. call `Encode()` on the typed resource; and
5. persist it with `store.Update` at the current resource version.

Removing the last finalizer completes deletion in the etcd store. Never remove
the finalizer if cleanup failed or ownership is ambiguous. If compatibility
with resources created before the finalizer was configured matters, have the
normal reconciliation path add a missing finalizer before creating external
state.

## 5. Wire the controller into the manager

Import the new package in
[`cmd/homelab-controller/main.go`](cmd/homelab-controller/main.go), construct
the reconciler and controller, and add a registration:

```go
secretReconciler := secretcontroller.NewReconciler(resourceStore, secretBackend)
secretController := controller.NewController(secretReconciler)

// In the []controller.Registration passed to controller.NewManager:
{
    Name:       "secret-controller",
    Controller: secretController,
    Watches: []controller.Watch{
        {
            Kind:   registry.SecretResource.Kind,
            Mapper: controller.IdentityMapper,
        },
    },
},
```

Use generated kind constants rather than duplicating string literals.

An identity watch is sufficient when only changes to the primary resource
matter. If `<Kind>` depends on another resource, also watch the dependency and
map its event to every affected primary resource. For the example, a
`SecretStore` change might use:

```go
{
    Kind:   registry.SecretStoreResource.Kind,
    Mapper: secretReconciler.RequestsForSecretStore,
},
```

`RequestsForSecretStore` should list `Secret` resources and return requests
only for those referencing the changed store. Test the mapper independently;
incorrect dependency mapping produces stale status that can look like a
reconciler bug.

The current manager reacts to etcd watch events; it does not automatically list
and enqueue every existing object when the process starts. If the new
controller must reconcile pre-existing resources after a restart even when no
new event occurs, add and test an explicit initial-list/resync mechanism as part
of the controller design.

## 6. Test every layer

### Schema and generated registration

Generation validates module metadata, schema names, references, duplicate
kinds and paths, and the combined OpenAPI document. Add focused registry tests
when the generated Go shape is non-obvious: construct `apigen.<Kind>Spec`, call
`registry.New<Kind>`, round-trip with `Encode` and `Decode`, and assert JSON field
names.

### Generic API behavior

Exercise at least create, get, list, update, invalid input, and deletion for the
new endpoint. Include operation-specific cases such as `If-Match`, patching, or
default finalizers when enabled. Existing shared-handler tests provide the
patterns.

### Reconciliation behavior

Use a fake `store.Store` and fake external backend. Cover at least:

- missing primary resource;
- successful first reconciliation;
- repeated reconciliation without duplicate external work;
- unchanged status without a write loop;
- changed spec and `observedGeneration`;
- missing or changed dependency resources;
- transient backend and store errors;
- stale resource-version conflicts;
- dependency event mapping; and
- safe finalization, including ownership mismatch and retryable cleanup failure.

If the controller talks to a real service, keep those tests separate from fast
unit tests and document how to start the dependency.

## 7. Update user-facing documentation and configuration

Add the new kind's semantics and ownership rules to `api-contract.md`. Update
`README.md` with a manifest, lifecycle description, and any local setup needed.
If reconciliation needs credentials, endpoints, policies, or containers, add
the corresponding typed configuration, validation, example configuration, and
deployment wiring. Do not read ad hoc environment variables deep inside the
reconciler when the existing configuration layer can own them.

For the example, a manifest might look like:

```yaml
apiVersion: homelab.io/v1alpha1
kind: Secret
metadata:
  name: database-password
spec:
  secretStoreRef:
    name: primary
  logicalPath: applications/database/password
  generator:
    length: 32
```

## 8. Verify the completed resource

Run the full local checks:

```sh
make generate
make fmt
make test
make vet
make build
git status --short
```

Running `make generate` a second time should produce no additional diff. That
checks that generated output is deterministic and committed.

With the API and its dependencies running, smoke-test the public boundary:

```sh
curl -i \
  -H 'Content-Type: application/yaml' \
  --data-binary @secret.yaml \
  http://127.0.0.1:8080/api/v1alpha1/secrets

curl -i \
  http://127.0.0.1:8080/api/v1alpha1/secrets/database-password
```

Confirm that invalid specs are rejected, ETags/resource versions behave as
expected, the controller reaches a stable status, repeated reconciliation is a
no-op, and deletion either completes immediately or remains pending until its
finalizer cleanup succeeds.

## Completion checklist

- [ ] Resource ownership and lifecycle are defined.
- [ ] An authored resource module contains the spec and optional status schema.
- [ ] Operations and finalizers are intentionally selected.
- [ ] Generated API, model, and registry files are refreshed but not hand-edited.
- [ ] Generated types encode and decode as expected.
- [ ] A controller is implemented only if reconciliation is required.
- [ ] Primary and dependency watches are registered.
- [ ] External operations, status updates, conflicts, and deletion are idempotent.
- [ ] Unit, API, mapper, finalizer, and integration tests cover the new behavior.
- [ ] API contract, README, configuration, and deployment examples are updated.
- [ ] Generation, formatting, tests, vetting, build, and a runtime smoke test pass.
