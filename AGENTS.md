# Stigmergy API and controller instructions

## Architecture

- Keep generic API resources separate from backend execution. ISO controllers
  render owned Pipeline resources; Pipeline controllers manage the provider;
  Concourse/Ansible operators execute work and report generic object status.
- Do not add bespoke Server action/reporting endpoints for behavior that belongs
  in `/servers/<name>/status`. Do not put SSH/provisioning fields in generic
  InventoryCaptureGroup schemas.
- Read `docs/rfcs/0003-server-reprovisioning-and-network-boot.md` and
  `docs/server-provisioning-rollout.md` before changing provisioning semantics.

## API writes and authentication

- Resource PATCH bodies are spec merge patches, not `{spec: ...}` envelopes.
  Use `If-Match: "<resourceVersion>"` for spec changes.
- Generic status PATCH uses a UID-bound envelope:
  `{metadata: {uid: "..."}, status: {...}}`, the same If-Match header, and
  `Content-Type: application/merge-patch+json`.
- `API_AUTH_FILE` provides opaque bearer-token identities. It is not JWT,
  OAuth or Basic auth. Preserve the existing policy's tokens during rollout;
  use `cmd/create-api-auth --policy-source <existing-policy> --output-dir
  <fresh-private-directory>` when preparing policy changes.
- Never disable auth in deployed environments or dump policies/Secrets. The
  unauthenticated override is development-only. Public boot/download paths are
  intentional exceptions; do not accidentally require an operator token in iPXE.
- Update source OpenAPI schemas and generated artifacts when changing API
  contracts. Keep Swagger auth usable with bearer tokens; do not invent a new
  custom login protocol or permission framework for this v1.

## Quick resource recipes

Base URL: `https://stigmergy.ryuugu.dev`. Swagger: `/docs/`; schemas:
`/openapi.json`; resources: `/api/v1alpha1/<collection>[/<name>]`.
Swagger's Authorize input takes the token without the `Bearer` prefix and
persists it across reloads.

Use the existing `ansible-roles/operators/common.py` API client with credentials
already loaded in the execution environment (`STIGMERGY_API_URL` is the base
URL, without `/api/v1alpha1`; `STIGMERGY_API_TOKEN` is the authorized token).
It handles bearer headers, HTTPS, JSON and status preconditions. Do not invent
`homelabc get/list` commands: that CLI currently provides `init` and `run`.

From the workspace root, a safe read-only summary is:

```sh
PYTHONPATH=ansible-roles/operators python3 - <<'PY'
import json
from common import API
api = API()
for collection in ('servers', 'machines', 'isos', 'pipelines', 'commands',
                   'inventory-capture-groups', 'ssh-certificate-authorities',
                   'ssh-key-pairs', 'ssh-certificates'):
    for item in api.list(collection):
        status = item.get('status', {})
        print(json.dumps({'collection': collection,
            'name': item['metadata']['name'], 'uid': item['metadata']['uid'],
            'generation': item['metadata']['generation'],
            'phase': status.get('phase'), 'conditions': status.get('conditions')}))
server = api.get('servers', 'beelink')
p = server['status'].get('provisioning', {})
print(json.dumps({'name': 'beelink', 'provisioning': {key: p.get(key) for key in
    ('activeRunRef', 'lastRunRef', 'lastSuccessfulRunRef', 'maintenance', 'provisioned')}}, indent=2))
PY
```

Collections also include `provisioning-runs`, `commands-pipelines`, `pipeline-providers`, `secrets`
and `secret-stores`. List returns an `{items: [...]}` envelope over HTTP;
`API.list` unwraps it. GET returns the resource directly. For Secrets, inspect
only metadata/conditions; never print `spec.data`. Use client-side name/label
filtering unless an API selector feature has actually been implemented.

Inside the same authenticated Python session, the write primitives are:

```python
from common import request_json
server = api.get('servers', 'beelink')
# Only perform a requested spec change; body is the spec merge patch itself.
updated = request_json(api.url + '/servers/beelink', api.token, 'PATCH', change,
    {'If-Match': '"' + server['metadata']['resourceVersion'] + '"',
     'Content-Type': 'application/merge-patch+json'})
# Only report actual observed state. This helper supplies UID and If-Match.
updated = api.patch_status(server, observed_status_patch)
```

`change` and `observed_status_patch` are the reviewed intended changes, not
complete replacement resources. Reread after a conflict; do not blindly replay
a destructive request or overwrite another operator's observations.

To CREATE an authorized resource, POST its full
`{apiVersion, kind, metadata: {name}, spec}` object to the collection with
`request_json(..., 'POST', object)`. For Command, use a new name each time and
`spec: {commandsPipelineRef: {name: 'servers'}, script: '<requested script>'}`.
Command specs are immutable disposable execution requests: rerun by creating a
new Command, not patching the previous script. Read the current schema before
creating any other kind. DELETE requires explicit scope/authorization; resolve
the named resource first and use its resourceVersion with If-Match.

## Trace and troubleshoot

- Server: inspect `status.machineRef`, management address/interface, `hostSSH`,
  `bootISORef`, trust digests and `provisioning.activeRunRef/lastRunRef`. Follow
  that UID-qualified ProvisioningRun for phase, snapshot, message and build.
- ISO: inspect phase, conditions, selected/completed build, artifacts and
  `pipelineRef`; follow that Pipeline to `spec.externalName`/provider.
- Command: inspect phase, conditions, `pipelineRef` and `buildID`; use that exact
  Concourse build for logs, not the newest unrelated build.
- Capture group: inspect `observedGeneration`, `inventory`, and
  `omittedResources`. A Partial group may still contain usable Servers.
- SSH CA/key/certificate: inspect Ready/conditions/observedGeneration and public
  trust/certificate state. Private keys are in Secrets/OpenBao, not debug output.
- Compare `status.observedGeneration` with metadata generation only for resource
  types that define it. Follow Server activeRunRef/lastRunRef to ProvisioningRun
  for checkpoints; lastSuccessfulRunRef remains the last verified installation.
- Check API `/readyz` first when the UI reports offline. For HTTP 502 inspect the
  reverse proxy's upstream and service/container state before changing resources.
  For 401 check the intended route's auth policy/token, not TLS bypasses.
- Machine `status.lastSeenTime` and bound Server `status.agent.lastSeenTime` use
  the report's API-assigned creation timestamp. The pulse is reporting freshness,
  not host/SSH availability. Reboots use ordinary Commands and the reviewed
  Ansible system-operation helper; see RFC 0005. Never infer reboot approval from
  adding UI code or observing a stale pulse.

## Provisioning status safety

- RFC 0003's desired lifecycle is authoritative: every new run boots a fresh
  pinned live image, even if already live on that build. Installed hosts use
  GRUB/iPXE; live hosts use generic kexec. Both consume shared ISO bootArguments,
  pinned in the run snapshot. Require a changed source/live boot ID before
  Installing. Never reuse a live session or substitute staging cleanup. Keep ISO boot details separate from lifecycle orchestration;
  fixes need failure-stage acceptance for both supported distros.

- Creating a ProvisioningRun with Server/Machine UIDs, reviewed serverGeneration
  and one discovered disk ID authorizes replacement. Enabling Server provisioning
  or ISO/CA/spec changes never does. Creation atomically reserves the Server.
- Preserve immutable attempt/snapshot ownership and dependency UIDs. Do not
  rewrite snapshots or mark success merely to recover a build.
- An interrupted installation is Blocked; no automatic rewipe is allowed.
  Explicit DELETE with If-Match may remove a Blocked run and atomically release
  its Server reservation. This changes API ownership only; a new ordinary run
  requires fresh disk confirmation and the fresh-live-boot lifecycle. Active
  runs and provisioning collection deletion are rejected.
  Explicit configuration repair can move the same Blocked attempt to
  AwaitingInstalled only with the owned live session, retained maintenance,
  installed boot target and no pending netboot. The external operator must
  verify the completed staged marker first. Fresh installed verification is
  still required before run success and reservation release.
- Status credentials are intentionally broad in v1. Do not confuse schema/state
  validation with proof of physical state; the operator supplies that evidence.

## Deploy and verify

- Run relevant Go tests (at least `go test ./internal/api` for API changes),
  gofmt and whitespace checks before pushing. Deploy the pushed revision, not an
  untracked local approximation.
- The deployed checkout is `/srv/homelab/stigmergy`. Fast-forward it; never
  reset away host changes. The infrastructure compose project is `infra`, with
  `/srv/homelab/docker-compose.yml` and a private bootstrap env file.
- For an API-only change, use the existing env file and
  `docker compose ... -p infra -f /srv/homelab/docker-compose.yml up -d --build
  --no-deps stigmergy`; do not redeploy unrelated services unnecessarily.
- Verify `/readyz` and the affected public/API behavior afterward. For Arch
  netboot verify rendered iPXE includes `net.ifnames=0` and MAC-selected BOOTIF.
- Keep private rollout helpers under ignored `.local/`; do not commit real
  credentials, deployment env files or private diagnostic output.
- The current private initialization config is
  `.local/deployment/run-model-cli.yaml`; it references the token-preserving
  `.local/deployment/homelab-init-runs.env`. Use that explicit config for init.
