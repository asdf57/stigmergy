# SSH management and ISO rollout

This implements RFC 0001's key/trust/image milestone and managed user-certificate
issuance/renewal. Only trusted administrators may create/edit SSHCertificate
resources: choosing a subject key and principals is signing authority. Never give the runner or Concourse
the CA private key. Nothing in this rollout promises that a compromised signer,
root-capable provisioning runner, or privileged image builder is harmless.

## 1. Deploy access controls first

The API process requires `API_AUTH_FILE`, a read-only mounted JSON policy.
Use independent, randomly generated tokens of at least 32 characters. The short
tokens below are deliberately invalid placeholders; replace all of them.

Run `make api-auth` in Stigmergy to generate the real policy and three distinct
tokens under gitignored `.local/api-auth/` (directory 0700, files 0600). It refuses
to overwrite existing identities. Append its `bootstrap.env` to the private
Docker env-file used by `homelabc init`; do not shell-source that JSON-bearing
file. Keep `agent-token` and `runner-token` separate for downstream enrollment.
Standalone Compose mounting instructions are in the Stigmergy README.

Every resource route is protected, including Secrets and certificate operations.
New routes require authentication by default. Only GET health/readiness, API
documentation, and iPXE routes are public. This is bearer-token authentication,
not HTTP Basic or a shared all-powerful token for all processes. Serve external
authenticated API traffic over HTTPS. Policy updates take effect after restart.

```json
{
  "identities": [
    {"name":"admin", "token":"ADMIN_TOKEN", "permissions":[
      {"kind":"*", "methods":["GET","POST","PUT","PATCH","DELETE"]}
    ]},
    {"name":"agent", "token":"AGENT_TOKEN", "permissions":[
      {"kind":"MachineReport", "methods":["POST"]},
      {"kind":"Server", "methods":["GET"]}
    ]},
    {"name":"runner", "token":"RUNNER_TOKEN", "permissions":[
      {"kind":"InventoryCaptureGroup", "methods":["GET"]},
      {"kind":"Server", "methods":["GET"]},
      {"kind":"SSHKeyPair", "methods":["GET"]},
      {"kind":"Machine", "methods":["GET"]},
      {"kind":"ISO", "methods":["GET"]},
      {"kind":"SSHCertificateAuthority", "methods":["GET"]},
      {"kind":"Command", "methods":["GET"]},
      {"kind":"Server", "subresource":"status", "methods":["PATCH"]}
    ]}
  ]
}
```

For tighter fleets, add `resourceNames` to the runner's Server permissions.
Named permissions cannot list a collection. Machine reports are discovery
claims, not proof of machine identity; never trust them as SSH host keys or
installed-trust attestations.

The API container runs as UID 65532. Its policy file must be readable by that
UID, not group/world writable, and mounted read-only. For the bootstrap role,
provide `STIGMERGY_API_POLICY` containing the complete JSON and
`STIGMERGY_API_TOKEN` containing its admin token. Bootstrap writes the policy
as UID/GID 65532, mode 0400. Existing deployments must install the same policy
and restart the API before creating CA Secrets. Do not use
`ALLOW_UNAUTHENTICATED_API=true` with real private material.

Bootstrap API tasks and `homelab-init/upload.sh` send the admin bearer token.
The web and Android clients accept a bearer token.

To test from Swagger, open `/docs/`, click **Authorize**, and paste the token
without the `Bearer ` prefix. **Try it out** automatically sends the standard
Authorization header. `/openapi.json` declares global HTTP bearer security and
401/403 responses for protected routes, including generated status routes.
Health/readiness and iPXE explicitly opt out. Swagger authorization is
persisted in browser local storage across page reloads; use **Logout** in the
Authorize dialog to clear it. The existing OpenAPI middleware handles request
validation; the small outer policy middleware handles token/allowlist checks.

## 2. Build and publish the changed projects

Publish your reviewed Stigmergy, homelabd, ansible-roles, and arch-provisioner
changes before pointing deployed pipelines at those revisions. Build/update
the runner image and homelabc CLI. This document does not commit, push, or deploy
anything for you.

Configure the API deployment:

| Setting | Purpose |
| --- | --- |
| `PUBLIC_API_URL` | HTTPS API address used by images and runners. |
| `ISO_ARTIFACT_BASE_URL` | HTTPS Copyparty base URL, with public ISO/netboot/manifest reads; writes require authentication. |
| `ISO_BUILDER_REPOSITORY` | Public HTTPS ansible-roles repository; defaults to the project repository. |
| `ANSIBLE_ROLES_REVISION` | Builder and provisioning source branch. |
| `ISO_DAEMON_REPOSITORY`, `ISO_DAEMON_REVISION` | Public HTTPS homelabd source and branch. |
| `ISO_UPLOAD_PASSWORD_VARIABLE` | Concourse credential variable, initially `file-registry`. |
| `COMMAND_RUNNER_IMAGE` | Updated arch-provisioner image. |
| `COMMAND_RUNNER_PARAMETERS` | JSON map of generic task parameters, including Concourse variable references. |

The full bootstrap Compose template supplies runner parameters referring to
`((stigmergy-runner-token))`, `((ssh/clients/ansible-runner.privateKey))`,
and `((ansible-runner-certificate))`. Bootstrap creates the runner-token Secret;
its value must match the runner API policy. Managed Server known_hosts is generated
from verified Server public identities, not a separate manually uploaded Secret.
SSHCertificateController owns the certificate Secret; do not upload a competing
manual Secret at `ansible-runner-certificate`. Connections use a Server-UID-bound
HostKeyAlias so trusting another fleet member cannot authenticate this Server.
Existing private API policies must grant the runner GET SSHKeyPair in addition to
its existing inventory/Server reads and Server/status PATCH. Preserve existing
token values when editing the policy and its private bootstrap env-file.
For existing standups, `go run ./cmd/create-api-auth --policy-source
.local/api-auth/api-access.json --output-dir .local/api-auth-operators` prepares
a fresh private directory with the same tokens and only the extra runner read.
Then use that directory with --bootstrap-env-source/--bootstrap-env-output to
compose a fresh private initialization env-file; original files are preserved.

Concourse's OpenBao policy permits these credentials, the runner key,
Git authentication, publishing credentials, and the host operator's AppRole
credentials. The separate ssh-host-operator policy grants reads only below
kv2/data/secrets/ssh/hosts/ plus revocation of its own short-lived tokens.
It cannot read CA signing keys. Bootstrap creates this AppRole and persists
its enrollment credentials at ssh-host-operator-auth without replacing them
on reruns. Apply the policies to
existing OpenBao deployments too; changing a checked-in policy does not revoke
permissions until OpenBao receives it. Bootstrap writes it on each bootstrap
run. Additional Git keys require deliberate policy grants, never a CA wildcard.
The publishing identity is privileged over its artifact destination; scope its
Copyparty volume/permissions appropriately. There is no signed-manifest or
verified-boot guarantee in this milestone.

## 3. Create the resources

Reuse the existing `SecretStore/openbao`, Concourse credential and
`PipelineProvider/concourse`. Grant the Git key write access to the configured
input repository. New homelab-init manifests create:

- `SSHKeyPair/homelab-ca-01` and `SSHKeyPair/ansible-runner`;
- `SSHCertificateAuthority/homelab-user-ca`;
- `SSHCertificate/ansible-runner`, a 24-hour user certificate for principal
  `ansible`, automatically renewed with eight hours remaining;
- `GitRepository/iso-build-inputs` (the dedicated `asdf57/iso-data` repository,
  branch `iso-build-inputs`);
- `ISO/debian-trixie-amd64` and `ISO/arch-rolling-amd64`;
- InventoryCaptureGroup/ssh-managed and Pipeline/reconcile-ssh-host-keys-ssh-managed.

ServerHostKeyController creates SSHKeyPair/server-host-<Server UID> automatically
using SERVER_HOST_KEY_SECRET_STORE (default openbao), at ssh/hosts/<Server UID>.
Do not create per-Server SSHKeyPair manifests. Its public projection is
status.hostSSH.keyPairRef/publicKey/fingerprint/keyReady; the external operator
separately reports installedKeyPairRef/installedFingerprint/observedGeneration
and phase. Missing or replaced established keys are not silently regenerated.

Apply through `homelab-init/upload.sh` using the admin token. Local
`groupVarsRef` conveniences are expanded from `GroupVars/*.yml` into ordinary
`groupVars` before upload; they are not an API resource or SSH-specific schema.
ISOController owns `Pipeline/iso-<name>`; do not apply these children yourself.

Create `asdf57/iso-data` and initialize its default branch with a README before
applying these manifests; the Git publisher needs a cloneable repository. Grant
the configured Git key write access. ISOController creates the input branch and
its owned directories, not the GitHub repository itself. It does not use the
inventory repository.

Git input publication uses UID ownership markers and refuses to overwrite
unowned directories.

Wait for key pairs and CA to be Ready, then the generated Pipeline. ISO Ready
requires a completed manifest matching the ISO lifetime and public inputs.
`status.completedBuild` records the producing Git revisions and bundle digest.
Source-only rebuilds select the newest matching completed build by start time;
Ready is not a claim that an in-progress newer build has already completed.

The managed certificate references the CA and subject SSHKeyPair, not a Server.
It owns `Secret/ssh-certificate-ansible-runner`, whose `data.value` is the public
OpenSSH certificate published at OpenBao path `ansible-runner-certificate`.
The signer uses only the CA private key internally; it does not read the subject
private key. Concourse receives the subject's private key and public certificate
through the existing explicit credential-variable references. New tasks resolve
the current published credential; already running tasks retain their copied file.

Certificate reconciliation runs on dependency changes and every 30 seconds.
It keeps the same certificate until renewal is due, the signer changes, or its
subject/principals/lifetime changes. Certificates are user-only, have bounded
lifetimes (5 minutes to 7 days), use random 64-bit serials, and permit PTY but not
forwarding/user-RC extensions. Referenced authority/key identities are pinned;
replacement requires explicit UID rebinding. Output storage location is fixed
for a resource lifetime. Only Ready external Secret publication makes the
certificate Ready. Monitor phase/expiry: an unavailable signer/store can prevent
renewal. Deleting the resource stops renewal and cleans its owned output but
does not revoke certificates already copied elsewhere before their expiry.

For manual homelabc runs, obtain the current public certificate from this
resource's status or its owned Secret and supply the matching runner private
key separately. The CLI does not autonomously renew local credential files.

## 4. Enroll and provision hosts

Server manifests select the authority and an Arch boot ISO. Change the selected
image explicitly for Debian hosts. Bootstrap-image selection is separate from
the installed operating system. iPXE refuses unresolved/replaced image identities
and incompatible authorities rather than falling back to distro aliases.

Live images embed the restricted agent token through Concourse's private
`stigmergy-agent-token` credential, never through Git. The installer creates
root-owned `/etc/homelabd` (0700) and `environment` (0600), containing
`API_TOKEN=<restricted-agent-token>`. Provisioning preserves this enrollment.
Token rotation requires rebuilding every image. Anyone able to read an image
can extract its token: filesystem permissions do not prevent offline extraction.
ISO and netboot downloads remain public by explicit operator choice. Anyone
downloading an image can use its agent token to submit MachineReports and read
Servers; this token is not a trusted proof of a particular machine's identity.
API authentication still protects other operations, and artifact writes require
the pipeline credential. No additional download-authentication flow is needed
for iPXE. A live-host source installation can receive
`HOMELABD_API_TOKEN_FILE=/absolute/token-file` and
`SSH_CA_BUNDLE_SOURCE=/absolute/public-bundle` when running `setup/install.sh`.
Do not copy the admin or runner token into the daemon.

For an interactive runner, pass homelabc `run` the absolute paths:

```text
--api-token-file /path/runner-token
--ssh-private-key-file /path/runner-key
--ssh-certificate-file /path/runner-key-cert.pub
```

Files are mounted read-only and must be readable by container UID 1000. Runner
initialization resolves the capture group and verified Server public identities
through the API and refuses unmanaged hosts. It never downloads host private keys.
An optional explicit --ssh-known-hosts-file supports non-Server administrative
inventories; ordinary commands never perform TOFU automatically.

Provisioning defaults to management access enabled, installs the same fixed
account service/timer and daemon into `/mnt`, preserves the separate agent
enrollment, and disables root SSH. Enrollment is checked before disk changes.
Ordinary users and their keys remain Ansible's responsibility.
The installed OS receives the verified managed live SSH host key to preserve
identity across its first reboot. Provisioning checks that the live private key
derives the desired public identity before copying it into /mnt.

The host-key operator runs every five minutes, or via a manual Concourse trigger.
For a fresh Server, it authenticates as ansible using accept-new on the first
SSH connection, persists the initial public pin through conditional Server/status
PATCH, then uses strict checking on retries. Only after pin persistence does it
read the managed private host key from OpenBao, install it atomically, validate
and reload sshd, and verify a fresh connection against ONLY the managed key.
The bootstrap pin is removed after verified convergence. Keys are staged in
private temporary directories, never pipeline outputs or Git.

This is TOFU: a first-contact impersonator can receive the managed private host
key. The public embedded agent token and its discovery claims do not authenticate
machines. V1 explicitly trusts first-contact network/address selection. No KVM
or console integration participates in enrollment.

Unexpected key changes stop the operator and commands. A planned fresh ISO boot
requires the administrator to pause the operator, GET the Server, then PATCH its
/status using metadata.uid and If-Match, clearing hostSSH.bootstrapPublicKey,
installedKeyPairRef and installedFingerprint to null and setting phase Pending.
Resume the operator only after checking the intended address/binding. Keep the
controller-owned keyPairRef/publicKey/fingerprint; reenrollment reinstalls the
same identity. There is no automatic reset on a verification failure.

The effective sshd configuration must not trust daemon-writable key sources.
Operator-supplied Match blocks and drop-ins must preserve this boundary.
Keep a tested console/recovery path. An `ansible` account without the
reconciler's root-owned identity ledger is intentionally rejected; do not
silently adopt or delete an unrecognized identity.

## 5. Rotate installed trust without provisioning disks

Add the opt-in label `homelab.io/ssh-management: enabled` to Servers before
bootstrap. One persistent Pipeline/reconcile-ssh-host-keys-ssh-managed has a
serial operator job and a Concourse time resource (5m). Its manifest lives in
homelab-init/Pipeline; upload.sh applies it and the existing generic Pipeline
controller configures Concourse. No API scheduler creates Commands for its ticks.
Each build consumes a Git-pinned ansible-roles revision and reads current API
state. After host-key verification, plays/ssh_trust.yml installs user-CA trust,
validates/reloads SSH, and reconnects using verified host keys and a CA certificate.
The Python operator then rereads current state and PATCHes both host identity
observations and installedSSHTrustBundleDigest through the Server's /status.
It uses the current If-Match revision, preserves unrelated writers, retries
status conflicts, and verifies the UID, generation, binding and desired digest
still match the state actually installed. Server/CA lifetime and desired digest
checks remain. A disk checksum alone or daemon report does not establish convergence.

All resource types with status schemas support the same generic PATCH endpoint.
Merge updates preserve unrelated fields; UID and ETag are required, schema
validation applies, and spec/other metadata edits are rejected. The runner has
broad Server-status permission for now, not field-level ownership enforcement.
It cannot edit Server spec. Treat it as a trusted status writer; finer-grained
status authorization is deferred.

Operator orchestration: ansible-roles/operators/ssh_host_keys.py; reusable API/SSH
primitives: operators/common.py; ordinary runner trust: operators/runner_trust.py;
installation: plays/ssh_host_keys.yml and roles/ssh_host_key. The runner's
profile.d/init.sh initializes operator credentials without requiring a prebuilt
known_hosts file; the operator constructs scoped trust before any Ansible call.
One pass is bounded, handles other targets after a per-target failure, and fails
the build for failed actionable targets. Pending key/address/CA dependencies are
reported as pending. Keep host-key operator groups nonoverlapping in v1.

Add a new key to `trustedKeyRefs`, retain the old signer during image and host
rollout, verify a controlled certificate signed by the new key, then switch the
signer. Keep old trust for offline hosts/media and outstanding certificates.
Signer-only changes do not publish new image inputs. Referenced keys, authorities,
and images cannot be retired through their protected lifecycle until consumers
are removed or changed.

## Validation boundary

When applying over an existing installation, stop/remove the previous recurring
trust executor and its owned requests/pipeline before activating the operator.
The manifest uploader upserts declared resources; removing a file alone does not
delete an already deployed resource. Do not run two host-key operators against
overlapping target groups.

Unit/integration tests exercise publication ownership/no-op/cleanup, resource
identity replacement, stale manifest selection, CA rotation, API permissions and
verified-trust reports. Disposable Debian/Arch tests exercise the account helper,
SSH certificates and rejection of daemon-forged keys. Android build/tests/lint
and Ansible syntax checks are separate client checks.

Before production, build both image recipes and boot disposable VMs. Verify
boot ordering, real timer repair with homelabd stopped, installed-OS provisioning,
host-key continuity across reboot, and an overlapping-key rotation. Container
helper tests do not prove systemd boot/timer execution or a bootable ISO.
