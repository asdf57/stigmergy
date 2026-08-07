# stigmergy

Homelab API attempt deux

## Run the stack

The API and its etcd datastore run together with Docker Compose:

```sh
make run
```

This builds the API image, starts etcd, waits for etcd to become healthy, and
then starts the API. Data is retained in the `etcd-data` Docker volume.

For a detached stack, use `make up`. Follow logs with `make logs` and stop the
stack with `make down`.

Once running:

- API readiness: <http://127.0.0.1:8080/readyz>
- OpenAPI document: <http://127.0.0.1:8080/openapi.json>
- Swagger UI: <http://127.0.0.1:8080/docs/>

Docker publishes the API on `0.0.0.0:8080`, so it is also reachable through
the host's LAN addresses. The etcd client port remains restricted to host
loopback.

`make run-local` remains available when intentionally running the API process
on the host against an independently managed etcd endpoint.

## Machine reports

The typed MachineReport API stores the latest observation for each stable
machine name:

- `POST /api/v1alpha1/machine-reports` creates a report resource.
- `GET /api/v1alpha1/machine-reports` lists the latest reports.
- `GET /api/v1alpha1/machine-reports/{name}` fetches one report.
- `PUT /api/v1alpha1/machine-reports/{name}` idempotently creates or replaces
  the latest report, making it the preferred endpoint for periodic agents.
- `DELETE /api/v1alpha1/machine-reports` deletes every report and returns the
  number deleted.
- `DELETE /api/v1alpha1/machine-reports/{name}` deletes a report using its
  current ETag in `If-Match`.

The OpenAPI request validator rejects missing required fields, unknown object
properties, invalid enums, out-of-range values, and malformed date-times before
the generated handler is called.

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
