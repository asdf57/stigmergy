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
The web and Android clients accept a session-only bearer token.

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
| `ISO_ARTIFACT_BASE_URL` | HTTPS Copyparty base URL; ISO artifacts and manifests require authentication. |
| `ISO_ARTIFACT_PASSWORD` | Private controller credential for reading Copyparty artifacts; supplied from `FILE_REGISTRY_PASSWORD`. |
| `ISO_BUILDER_REPOSITORY` | Public HTTPS ansible-roles repository; defaults to the project repository. |
| `ANSIBLE_ROLES_REVISION` | Builder and provisioning source branch. |
| `ISO_DAEMON_REPOSITORY`, `ISO_DAEMON_REVISION` | Public HTTPS homelabd source and branch. |
| `ISO_UPLOAD_PASSWORD_VARIABLE` | Concourse credential variable, initially `file-registry`. |
| `COMMAND_RUNNER_IMAGE` | Updated arch-provisioner image. |
| `COMMAND_RUNNER_PARAMETERS` | JSON map of generic task parameters, including Concourse variable references. |

The full bootstrap Compose template supplies runner parameters referring to
`((stigmergy-runner-token))`, `((ssh/clients/ansible-runner.privateKey))`,
`((ansible-runner-certificate))`, and `((ansible-known-hosts))`.
Create the runner-token and known-hosts credentials as `Secret` resources with
data `{"value":"..."}` and paths matching those variable names at `SecretStore/openbao`.
SSHCertificateController owns the certificate Secret; do not upload a competing
manual Secret at `ansible-runner-certificate`. The runner token must match
its API policy identity. Provision known hosts from independently
verified host keys, not an unverified `ssh-keyscan` or daemon report.
Key known-hosts entries by Server name, using `HostKeyAlias=inventory_hostname`,
not merely by a discovered IP address: an agent must not redirect a Server's
provisioning to another trusted fleet member. The shipped server variables and
trust playbook set that alias. Preserve this invariant in custom inventories.

Concourse's OpenBao policy now permits only these credentials, the runner key,
Git authentication, and publishing credentials. Apply the revised policy to
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
- a generic `ssh-trust` inventory group and CommandsPipeline with schedule/commandTemplate (fresh disposable Commands are created each interval).

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
The Copyparty `iso-resources` volume therefore requires authentication, and the
publisher refuses anonymous-readable storage. Download ISOs with the `pipeline`
account. Unauthenticated iPXE artifact downloads cannot boot these private images;
authenticated netboot delivery must be configured separately, without exposing
storage passwords in public boot scripts. A live-host source installation can receive
`HOMELABD_API_TOKEN_FILE=/absolute/token-file` and
`SSH_CA_BUNDLE_SOURCE=/absolute/public-bundle` when running `setup/install.sh`.
Do not copy the admin or runner token into the daemon.

For an interactive runner, pass homelabc `run` the absolute paths:

```text
--api-token-file /path/runner-token
--ssh-private-key-file /path/runner-key
--ssh-certificate-file /path/runner-key-cert.pub
--ssh-known-hosts-file /path/verified-known-hosts
```

Files are mounted read-only and must be readable by container UID 1000. Runner
initialization fails closed if credentials are missing. It never enumerates
Server key references or downloads arbitrary private Secrets.

Provisioning defaults to management access enabled, installs the same fixed
account service/timer and daemon into `/mnt`, preserves the separate agent
enrollment, and disables root SSH. Enrollment is checked before disk changes.
Ordinary users and their keys remain Ansible's responsibility.
The installed OS receives the verified live SSH host keys to preserve identity
across its first reboot. A later fresh live-image boot generates new host keys
and needs trusted-console verification/enrollment again; unattended SSH host
certificate issuance is not implemented by this milestone.

The effective sshd configuration must not trust daemon-writable key sources.
Operator-supplied Match blocks and drop-ins must preserve this boundary.
Keep a tested console/recovery path. An `ansible` account without the
reconciler's root-owned identity ledger is intentionally rejected; do not
silently adopt or delete an unrecognized identity.

## 5. Rotate installed trust without provisioning disks

After confirming a Server is installed/enrolled, add the opt-in label
`homelab.io/ssh-management: enabled`. The dedicated `ssh-trust` CommandsPipeline
creates a fresh disposable Command every five minutes from commandTemplate,
with one-day TTL, invoking only `plays/ssh_trust.yml`. It skips overlapping
scheduled runs. Completion, build IDs and cleanup are tracked per request.
The playbook installs trust, validates/reloads SSH, reconnects using verified
host keys and a CA certificate, and PATCHes the Server object's status through
`/api/v1alpha1/servers/<name>/status`. It sends `If-Match` with the verified
snapshot's resourceVersion and a body containing `metadata.uid` plus
`status.installedSSHTrustBundleDigest`. Server/CA lifetime and desired digest
checks remain. A disk checksum alone or daemon report does not establish convergence.

All resource types with status schemas support the same generic PATCH endpoint.
Merge updates preserve unrelated fields; UID and ETag are required, schema
validation applies, and spec/other metadata edits are rejected. The runner has
broad Server-status permission for now, not field-level ownership enforcement.
It cannot edit Server spec. Treat it as a trusted status writer; finer-grained
status authorization is deferred.

Add a new key to `trustedKeyRefs`, retain the old signer during image and host
rollout, verify a controlled certificate signed by the new key, then switch the
signer. Keep old trust for offline hosts/media and outstanding certificates.
Signer-only changes do not publish new image inputs. Referenced keys, authorities,
and images cannot be retired through their protected lifecycle until consumers
are removed or changed.

## Validation boundary

Unit/integration tests exercise publication ownership/no-op/cleanup, resource
identity replacement, stale manifest selection, CA rotation, API permissions and
verified-trust reports. Disposable Debian/Arch tests exercise the account helper,
SSH certificates and rejection of daemon-forged keys. Android build/tests/lint
and Ansible syntax checks are separate client checks.

Before production, build both image recipes and boot disposable VMs. Verify
boot ordering, real timer repair with homelabd stopped, installed-OS provisioning,
host-key continuity across reboot, and an overlapping-key rotation. Container
helper tests do not prove systemd boot/timer execution or a bootable ISO.
