# RFC 0001: ISO pipelines and SSH management access

- Status: Proposed
- Created: 2026-09-28
- Updated: 2026-10-04
- Scope: Stigmergy resources, ISO builds, management-account reconciliation,
  and SSH trust

## Summary

The immediate goal is to create SSH CA key pairs, resolve their public trust
bundle, and build that bundle into bootable images. Reuse the existing key,
secret, Git, and pipeline resources. Add ISO orchestration and CA trust
projection; use a managed SSHCertificate for user-certificate issuance and renewal.

The management identity is always `ansible`. A root-owned local service ensures
the account exists at boot and periodically at runtime. The image builders,
Ansible provisioning, and `homelabd/setup/install.sh` install the same service
and timer. The unprivileged daemon has no account-creation or elevation
interface.

ISOController publishes resolved public build inputs to Git and owns a generated
Pipeline. Concourse builds when those inputs or builder sources change.
Immutable artifacts and a manifest establish build completion.

The implementation includes CA
trust projection, ISO orchestration, authenticated API access, and local account
reconciliation, plus managed user-certificate issuance and automatic renewal.
See the [rollout guide](../ssh-management-rollout.md) for deployment
requirements and the remaining real ISO/VM/Concourse validation boundary.

## Responsibilities and resources

| Resource | Created by | Responsibility |
| --- | --- | --- |
| SecretStore | Operator/bootstrap | Describe the external secret store. |
| UsernamePasswordCredential | Operator/bootstrap | Supply the existing pipeline-provider credential. |
| PipelineProvider | Operator/bootstrap | Describe the pipeline service, team, and credential reference. |
| GitRepository | Operator/bootstrap | Describe the Git destination and authentication for generated inputs. |
| SSHKeyPair | Operator/bootstrap | Own one stable key pair and its backing Secret. |
| Secret | Key-pair or credential controller | Manage private material at the selected SecretStore. |
| SSHCertificateAuthority | Operator/bootstrap | Select a signer and publish trusted public keys. |
| ISO | Operator/bootstrap | Declare an image, its CA, and build destinations. |
| Pipeline | ISOController or CommandsPipelineController | Manage generated external pipeline configuration through PipelineController. |
| CommandsPipeline | Operator/bootstrap | Reusable executor settings and optional schedule/command template. |
| Command | Operator or scheduled executor | Immutable one-shot execution, build observation, and optional completion TTL. |
| Server | Operator/bootstrap | Select boot artifacts and desired installed-system trust. |
| SSHCertificate | Trusted administrator/bootstrap | Maintain a user certificate and renew its public Secret output. |

There is no separate trust-bundle, signing-request, ISO-build-run, or generic
workflow resource initially. Command is the one-shot execution resource for
provisioning scripts. Git snapshots, artifact manifests, and local systemd
units are implementation artifacts.

```text
SSHKeyPair ──owns──> Secret ──references──> SecretStore
     ▲
     │ key references
SSHCertificateAuthority ──publishes──> public trust bundle
     ▲                                      │
     │ authority reference                  ├──> ISOController
SSHCertificate                              └──> Server trust projection

ISOController ──publishes──> Git input snapshot ──triggers──> Concourse build
      │                                                         │
      └──owns──> Pipeline ──PipelineController──> Concourse       │
      ▲                                                         ▼
      └────────────observes──────── immutable artifacts and manifest

systemd boot/timer ──> fixed account reconciler ──> ansible account
homelabd ──> discovery and observations
authorized runner ──> certificate-authenticated SSH ──> Ansible provisioning
```

Key-pair reconciliation ends at key-pair status. Server consumers resolve the
keys or authorities they reference. SSHKeyPairController has no Server
management-access projection responsibilities.

## 1. Configure shared infrastructure

Reuse `SecretStore/openbao`, `PipelineProvider/concourse`, and the provider's
credential resource. Add or reuse a `GitRepository/iso-build-inputs` and a
separate Git authentication key if necessary.

Artifact upload/download endpoints and credentials remain deployment
configuration initially. Use the existing Copyparty publishing path for the
first implementation; keep manifest contents independent of that transport.
A separate artifact-store resource can be introduced when independent
destinations require their own lifecycle.

Provider configuration contains connection details, not ISO recipes. Private
credentials never enter public build-input files.

## 2. Create stable SSH key pairs

```yaml
apiVersion: homelab.io/v1alpha1
kind: SSHKeyPair
metadata:
  name: homelab-ca-01
spec:
  algorithm: ed25519
  secretStoreRef: {name: openbao}
  path: ssh/authorities/homelab/01
---
apiVersion: homelab.io/v1alpha1
kind: SSHKeyPair
metadata:
  name: ansible-runner
spec:
  algorithm: ed25519
  secretStoreRef: {name: openbao}
  path: ssh/clients/ansible-runner
```

SSHKeyPairController generates material, creates an owned Secret, waits for
SecretStore publication, and exposes the public key, fingerprint, and
UID-qualified Secret reference in status.

A key pair represents one cryptographic identity. Rotation creates another
resource; do not silently replace key material under the same name. CA signing,
runner authentication, and Git authentication use distinct keys and permissions.

## 3. Resolve authority trust

```yaml
apiVersion: homelab.io/v1alpha1
kind: SSHCertificateAuthority
metadata:
  name: homelab-user-ca
spec:
  signingKeyRef: {name: homelab-ca-01}
  trustedKeyRefs:
    - name: homelab-ca-01
status:
  phase: Ready
  observedGeneration: 1
  signingKeyRef: {name: homelab-ca-01, uid: "..."}
  trustBundle:
    - keyPairRef: {name: homelab-ca-01, uid: "..."}
      publicKey: ssh-ed25519 AAAA...
      fingerprint: SHA256:...
  trustBundleDigest: sha256:...
```

CAController references user-created keys and owns no additional Secret.
It requires one signer included in the trusted set and resolves every key.
Missing dependencies produce a condition; they never silently remove a key
from the desired bundle.

Construct canonical public-key content with stable ordering and no duplicate
material. Comments and reference-list order do not affect the bundle digest.
Changing only the signer does not rebuild images or reinstall trust.

The bundle remains a public output in CA status. An independently managed
bundle resource is deferred until consumers need trust sets combining multiple
authorities.

## 4. Declare an ISO and publish its inputs

```yaml
apiVersion: homelab.io/v1alpha1
kind: ISO
metadata:
  name: debian-trixie-amd64
spec:
  distribution: debian
  version: trixie
  architecture: amd64
  bootMode: uefi
  sshCertificateAuthorityRef: {name: homelab-user-ca}
  pipelineProviderRef: {name: concourse}
  buildInputs:
    repositoryRef: {name: iso-build-inputs}
    branch: main
    path: images/debian-trixie-amd64
status:
  phase: Pending
  observedGeneration: 1
  pipelineRef: {name: iso-debian-trixie-amd64, uid: "..."}
  inputRevision: "..."
  desiredTrustBundleDigest: sha256:...
```

One ISO describes one distribution/release/architecture/boot-mode combination.
ISO and netboot files may be outputs of the same recipe.

ISOController resolves the CA and publishes:

```text
images/debian-trixie-amd64/
├── image.yaml
└── ssh-user-ca.pub
```

The configuration includes the ISO UID, resolved image settings, trust-bundle
digest, and build-affecting recipe configuration. It contains no private keys.
Commit configuration and bundle together, and commit only when generated
content changes. Unrelated status updates must not produce new input versions.

Reuse the existing Git publisher through a shared package/interface rather than
making ISOController depend on inventory-publication reconciliation. Enforce
exclusive ownership of generated paths and reject conflicting publishers.
Git unavailability leaves input publication Pending or Failed with a condition.

A Git revision records the published snapshot. For shared branches, unrelated
commits outside the owned directory must not invalidate an image: compare the
owned input content as well as retaining the fetched revision for provenance.

## 5. Generate a provider-specific Pipeline

ISOController creates one owned child:

```yaml
apiVersion: homelab.io/v1alpha1
kind: Pipeline
metadata:
  name: iso-debian-trixie-amd64
spec:
  providerRef: {name: concourse}
  externalName: iso-debian-trixie-amd64
  definition:
    format: concourse
    data: |
      # Generated resources and tasks invoking versioned build scripts.
```

The controller knows the ISO build steps and delegates rendering to the
selected provider implementation. Initially support Concourse and report
unsupported types explicitly. PipelineController owns all external provider
configuration calls; it has no CA or distribution-specific logic.

Keep task bodies in versioned builder scripts. Generated pipeline YAML fetches
inputs, invokes scripts, and publishes results. Reuse small rendering and
publication interfaces without introducing a universal workflow language.

The child uses the owner-UID annotation/finalizer pattern until generic
ownership is implemented. CommandsPipeline is now reusable executor settings;
individual Commands own their own execution Pipelines. A conflicting child is never
silently adopted.

## 6. Let Concourse schedule builds

The generated pipeline watches its image-input directory with a Git resource
and `trigger: true`. Builder and daemon source resources can also trigger it.

```text
Trust content changes
→ ISOController publishes changed public inputs
→ Git resource discovers a new version
→ Concourse fetches that snapshot and builds
```

Build-affecting template changes must alter a watched source or the generated
input snapshot. Applying different pipeline YAML alone is not the build
trigger contract.

Concourse owns execution, retries, and history. PipelineController continues
managing configuration; there is no runOnChange field, last-triggered state, or
custom Stigmergy Concourse resource initially.

The builder consumes image configuration, a public CA-bundle file, and daemon
artifacts. It performs no live API lookup. The same builder can run locally or
under another executor using the same files.

## 7. Install management access and reconcile runtime deletion

Both live images and installed systems contain:

```text
/etc/ssh/homelab-user-ca.pub
/etc/ssh/sshd_config.d/…management configuration…
/etc/sudoers.d/…ansible policy…
/usr/local/libexec/ensure-ansible-user
/etc/systemd/system/ansible-account.service
/etc/systemd/system/ansible-account.timer
…homelabd.service
```

The account service is a root-owned oneshot with one fixed operation: ensure
`ansible` exists with a normal non-root UID, its private group, /home/ansible,
and the selected fixed shell. It accepts no arguments or network input.

Run it during bootstrap before management access is needed and periodically
afterward, initially about once per minute. Use a timer that invokes the service
again after it becomes inactive; do not configure the oneshot to remain active.
Ordering must not prevent the timer from repairing account deletion while sshd
is already running.

Deleting ansible at runtime causes the next invocation to recreate it, even
when homelabd is stopped or Stigmergy is unreachable. Account creation is
idempotent and serialized. An incompatible existing identity, unsafe home path,
or UID/GID collision reports a conflict instead of taking over another account
or recursively changing ownership. Define and test safe reuse of the original
local UID/GID and residual home directory during implementation.

The reconciler only repairs the fixed account. It does not install keys, change
CA trust, accept arbitrary commands, or manage ordinary users. Use absolute
executable paths, a controlled environment, and root-owned configuration.
Recovery assumes the privileged service remains installed and enabled; root
can deliberately disable it.

Install the static sudo rule:

```sudoers
ansible ALL=(ALL:ALL) NOPASSWD: ALL
```

Management SSH accepts the intended CA-signed user certificates. Disable
ordinary authorized-key files/commands, password authentication, and
keyboard-interactive authentication for ansible. Configure TrustedUserCAKeys
with the root-owned bundle and scope CA trust to the intended login policy.

Disable password login without making the account ineligible for certificate
login under distribution account/PAM policy. Verify Arch and Debian behavior
before choosing the password marker.

homelabd remains unprivileged with NoNewPrivileges=yes. It reports discovery and
observations, optionally including account-service failures. It cannot start a
privileged request with parameters or modify the reconciler, timer, SSH
configuration, sudoers, or account databases.

No account, including ansible, may authenticate from a daemon-writable
authorized-key source. Otherwise daemon compromise could authenticate as root or another
sudo-capable user. Ordinary users' authorized keys are installed by Ansible.
Run the fixed reconciler from SSH's ExecStartPre as well as the independent
timer so initial SSH startup does not race account creation.

### Required homelabd installer support

The homelabd project must own the reusable account-reconciliation executable
or script and its service/timer definitions under its setup assets.

Extend `homelabd/setup/install.sh` to write/install these assets on the target
host, not just install homelabd.service. The installer must:

1. Install the reconciler and service/timer files with root ownership and modes
   that prevent modification by homelabd or ordinary users.
2. Install and validate the static ansible sudo policy, with compatible-account
   checks before granting access to an existing identity.
3. Reload systemd, run account reconciliation immediately, and enable/start
   the periodic timer so the account is created during installation and repaired
   after later deletion.
4. Fail clearly when reconciliation or timer activation fails, and preserve
   idempotent behavior on subsequent installer runs.

Installing the account does not establish SSH trust. Management SSH activation
must use a validated, operator/provisioner-supplied CA bundle; the installer
must not create a password or fallback login key when that bundle is absent.

ISO builders and Ansible must reuse these homelabd setup assets so all three
installation paths have the same policy. Image assembly copies/enables units
in the target filesystem without starting services on the build host. Ansible
installs them in the new OS before reboot. The live-host installer runs the
initial reconciliation and timer on the host being installed.

This is planned work in homelabd; this RFC does not itself implement it.

## 8. Publish and observe completed artifacts

Upload immutable ISO/netboot files, then publish a manifest:

```json
{
  "isoUid": "...",
  "inputRevision": "...",
  "trustBundleDigest": "...",
  "sourceRevisions": {
    "builder": "...",
    "homelabd": "..."
  },
  "artifacts": [
    {
      "type": "iso",
      "url": "https://files.example/iso/immutable-build.iso",
      "sha256": "..."
    }
  ]
}
```

ISOController reads manifests and accepts only results for its resource lifetime
and desired input content. Preserve the fetched revisions for provenance.
Pipeline Ready means configured; ISO Ready requires a matching completed
artifact. Keep the previous successful artifact visible while a replacement
is Pending.

Publish results per build. Any mutable latest alias requires ordered or
conditional promotion so older results cannot overwrite newer ones. Specify
selection among builds for the same inputs but different source revisions.
Without provider outcome tracking, an absent matching manifest means Pending,
not proof that execution failed; Concourse exposes those failures and retries.

## 9. Select boot artifacts and installed-system trust

```yaml
apiVersion: homelab.io/v1alpha1
kind: Server
metadata:
  name: beelink
spec:
  boot:
    isoRef: {name: debian-trixie-amd64}
  sshCertificateAuthorityRef: {name: homelab-user-ca}
  # Existing machineSelector and installed-OS configuration omitted.
status:
  desiredSSHTrustBundleDigest: sha256:...
  installedSSHTrustBundleDigest: sha256:...
```

The proposed boot.isoRef selects bootstrap artifacts separately from the
installed operating system. Replace distro-only iPXE selection with resolution
of the selected ISO's completed netboot artifacts. Initially require the ISO
and Server to reference the same authority.

Bundle revisions may differ during rotation; the runner must possess a
certificate accepted by the chosen image, not merely a matching authority
name. User certificates also do not replace SSH server-identity verification.

ServerController projects desired trust when its authority changes. Schedule a
dedicated trust-update playbook through the provisioning workflow; never invoke
disk provisioning just to update trust. Ansible installs the bundle atomically,
validates/reloads sshd, and reports the installed content. Verify rollout through
authenticated access, not only homelabd reports.

Initially use an opt-in inventory selector and a reusable CommandsPipeline
with a generic periodic schedule and commandTemplate. Each interval creates a
fresh disposable Command running only the trust playbook. Deployment-supplied generic runner parameters provide scoped API and
SSH credentials; inventory and pipeline schemas contain no SSH-management
fields. The generic resource `/status` PATCH subresource records the checked
digest after reconnection. UID and If-Match bind it to the verified Server
snapshot; authority identity and current desired trust are rechecked. Status
permissions are separate from spec permissions. Initially the trusted runner
has broad Server-status permission; field-level ownership is deferred.

Live images embed only the restricted agent API token, injected through private
Concourse credentials rather than Git. The shared installer creates root-owned
/etc/homelabd (0700) and its environment file (0600); disk provisioning preserves
the enrollment. ISO and netboot artifacts must require authenticated reads:
anyone who downloads an image can extract its token. The publisher fails closed
if storage allows anonymous reads. Token rotation requires rebuilding all images.
Unauthenticated iPXE downloads cannot consume these private artifacts; authenticated
netboot delivery is a separate requirement, without public storage credentials.
Concourse's external-store policy must explicitly exclude CA private material.

The management username and managed-Linux inventory ansible_user remain fixed
to ansible. Ordinary user creation belongs to the provisioning playbook.

## 10. Add authorized certificate issuance

Use one managed resource rather than a separate SSHCASigningRequest:

```yaml
apiVersion: homelab.io/v1alpha1
kind: SSHCertificate
metadata:
  name: ansible-runner
spec:
  authorityRef: {name: homelab-user-ca}
  keyPairRef: {name: ansible-runner}
  principals: [ansible]
  ttl: 24h
  renewBefore: 8h
  secretStoreRef: {name: openbao}
  path: ansible-runner-certificate
status:
  phase: Ready
  serial: "123"
  validAfter: "..."
  validBefore: "..."
  certificate: ssh-ed25519-cert-v01@openssh.com AAAA...
```

The authenticated API admits certificate writes only for privileged operators;
daemon and runner policies cannot create/edit certificates. Principal selection
is signing authority, not an ordinary user permission. The controller issues
user certificates only, with explicit principals and TTL bounded from 5 minutes
to 7 days. The management manifest uses ansible; other privileged operator
policies can use other principals without changing the fixed account.

There is no serverRef. The certificate authorizes login wherever its issuing
key and principal are accepted, with proof of possession of the subject private
key. The runner receives its own private key and certificate; only the signer
needs the CA private key.

Reconcile on dependencies and every 30 seconds. Preserve a published certificate
until its renewal deadline or a change to signer, subject, principals, or
lifetime. An owned Secret persists the signed public result before status
publication, so retries do not continually extend validity. Use random 64-bit
serials and pin authority/subject resource identities; do not read the subject
private key. The output path/store is fixed for a resource lifetime. Concourse
reads the current public certificate from its existing credential variable.
Deleting the resource stops renewal and cleans its output, but does not revoke
already copied certificates. User certificate renewal does not rebuild ISOs;
only changes to trusted public keys do that. Host certificates remain future work.

## 11. Provision and rotate

Before reboot into the installed OS, Ansible installs the account reconciler
and timer, management SSH policy, public bundle, and daemon. It provisions
ordinary users separately.

Rotation is explicit:

1. Create the new CA key pair.
2. Add it to trustedKeyRefs, retaining the old signer.
3. Rebuild images and update installed-server trust.
4. Verify convergence and acceptance of the new key.
5. Switch signingKeyRef; new certificates use it without another bundle rebuild.
6. Remove old trust once consumers and certificate lifetimes permit.
7. Delete unreferenced key resources when certificate validity and trust retention permit.

There is no keyGeneration counter or automatic retirement timer. During the
pre-switch validation step, use a controlled test certificate signed by the new
key; do not silently change ordinary issuance policy.

Offline machines and old media may still require the old signer. Retiring that
trust before updating them requires explicit recovery or retirement of those
consumers. Emergency compromise response may sacrifice availability. Existing
sessions are not automatically terminated by certificate expiry or trust removal.

## Ownership and security boundaries

Key pairs own Secrets. Authorities and certificates reference keys without
deleting them. ISO owns its Pipeline and generated input path; deleting it
cleans up those owned objects/files, not the repository, provider, or CA.
Artifact retention remains a separate policy.

Use UID-qualified observations and explicit reference/deletion checks. Storing
a UID in status alone does not prevent replacement: controllers must reject
unexpected identity changes or require authorized rebinding.

homelabd must not be authorized to read Secrets, issue certificates, change CA
or key resources, edit pipelines, publish artifacts, or change Server trust.
The builder receives public trust and scoped publication credentials only.
API access controls are required before production CA private keys enter the
current Secret path, whose spec can contain private material.

Administrative API writers, signers, provisioning runners, build systems, and
publishers remain trusted. This design confines the daemon and limits coupling;
it does not make compromise of those privileged components harmless.

## Implementation milestones and validation

1. Establish private-material access controls; refactor CA schema/controller to
   resolve existing key pairs and publish canonical trust.
2. Implement the fixed account reconciler and timer in homelabd, including
   installer integration and reusable image/provisioning assets.
3. Add ISO input publication, provider rendering, and owned Pipeline lifecycle.
4. Update builders to consume a CA bundle, install management access, and publish
   immutable artifacts/manifests. Verify login with a controlled test certificate.
5. Add Server boot selection and installed-system trust update integration.
6. Add operator-authorized managed certificates, renewal, and runner delivery.

Validate unchanged reconciliation produces no new Git commits or rebuilds;
trust changes produce a new snapshot; signer-only changes do not. Verify
stale builds cannot mark current inputs Ready.

On both supported distributions, test initial installation, repeated installation,
boot-time account creation, runtime deletion/recreation with homelabd stopped,
and safe failure on identity/home-directory conflicts. Test certificate login
and rejection of password/raw-key alternatives. Test planned rotation through
overlapping trust on both live and installed systems.

## Remaining decisions

- Local UID/GID retention and safe home recovery after account deletion.
- Artifact discovery and ordered promotion, including source-triggered builds.
- Scheduling the dedicated trust-update provisioning workflow.
- Certificate revocation, historical audit retention, and future non-admin issuance policy.
- SSH host-key verification and recovery for offline machines or stale media.

## Disposable Command execution refinement

CommandsPipeline stores reusable repository, inventory group and provider references. Command references CommandsPipeline explicitly, contains an immutable script and optional ttlSecondsAfterFinished, and represents one execution—not a mutable script slot. Multiple Commands/executors may target the same group.

CommandsPipeline owns one persistent generic Pipeline with one Concourse run job and a stable commands-<executor-name> Git branch. CommandController snapshots accepted settings, publishes a UID-owned script directory and records its exact commit. A CAS on CommandsPipeline.status.activeCommandRef admits one active Command; others wait without changing the shared job. The active Command installs its accepted settings, script path and pinned version and holds the slot until its build is terminal. PipelineController applies external configuration and cleanup. No Git/time triggers execute commands.

The lifecycle is Pending → Dispatching → Running → Succeeded/Failed, with build ID, shared pipeline reference, revision and completion time. Register the exact Git version before submission. Persist Dispatching and the previous build ID with a CAS, then use Concourse's JSON job-build endpoint to obtain the new build ID, reusing standard fly login authentication. On response loss, adopt only a sole newer build; never resubmit automatically. Missing or ambiguous results keep the executor slot reserved until operator investigation. Exactly-once execution is not claimed. Because the job-build endpoint lacks per-build input overrides, execution is serialized per executor to protect queued builds from configuration races. Different executors are independent. Managed jobs must not be triggered or edited manually.

Post-completion TTL deletes only that request. Its finalizer aborts and observes only its build, removes its UID-owned Git directory, releases its executor slot and permits object deletion. The shared Pipeline, other requests and Git history remain. Omitted TTL retains execution history. Executor deletion is blocked by referencing Commands and then deletes its owned Pipeline. New requests are explicit retries; no automatic build retry.

For recurring trust reconciliation, schedule plus commandTemplate creates a fresh Command each interval. Persist a scheduling cursor before creation; deterministic names, no backfill and skipping overlapping scheduled requests prevent replay across restart/TTL cleanup. A crash between claiming and creating can miss an interval. Thirty-second polling is approximate scheduling, not a precise timer. Ad-hoc Commands can overlap; scheduled overlap suppression applies only to that executor's scheduled requests.

Tradeoff: a pipeline per retained Command costs provider objects but avoids shared-script/version races and reuses generic pipeline ownership. One-day TTL bounds retained trust-reconciliation objects; shorter retention may be chosen. Do not continuously reapply TTL-expiring Command manifests: creating a new UID is a new execution request.
