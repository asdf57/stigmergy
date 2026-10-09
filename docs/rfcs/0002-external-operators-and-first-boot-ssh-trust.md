# RFC 0002: External reconciliation operators and first-boot SSH trust

- Status: Implemented in source; deployment and real-machine rollout pending
- Created: 2026-10-05
- Scope: recurring external reconciliation, Server-owned SSH identities,
  and first-boot trust
- Related: [RFC 0001](0001-iso-pipelines-and-ssh-management-access.md)

## Summary

Use persistent Concourse pipelines as external reconciliation executors. Each
pipeline has an operator job that runs periodically and can also be triggered
directly. A build reads current API state, inspects its targets, applies an
idempotent operation, verifies the result, and updates resource status.

The API stores desired and observed state. Resource controllers manage resource
relationships and external pipeline configuration. External operators perform
machine-side work. These responsibilities need not run in separate processes
initially; their contracts should remain separate.

Creating a Server should automatically create a controller-owned SSHKeyPair.
Operators install that key; runners derive known_hosts from API-managed public
identities. Users do not create a separate host SSHKeyPair manifest per Server.

For v1, use trust on first use (TOFU) for the initial SSH connection on the
operator's trusted management network, followed by strict managed-key checking.
A public image and its embedded shared agent token are not machine identity.
TOFU is an explicit first-contact risk acceptance, not authenticated enrollment.
Console/KVM automation is outside this design.

## 1. Current baseline and proposed work

Already implemented:
- Public ISO/netboot artifacts with immutable build manifests and hashes.
- A shared restricted agent token embedded in images.
- Fixed ansible management login, user-certificate authentication, and local
  account-reconciliation service/timer.
- Live-image SSH host-key generation at boot.
- Generic Pipeline/PipelineProvider, inventory capture groups, disposable
  Commands, private OpenBao credentials, and generic resource /status updates.
- An authenticated runner and strict SSH host-key checking.

Implemented by this RFC:
- Automatic Server-owned host SSHKeyPairs and their lifecycle.
- Operator pipeline definitions and host-key installation/reconciliation.
- Automatically derived runner known_hosts.
- Durable first-seen bootstrap-key recording and the scoped TOFU transition.

Recurring operator scheduling belongs to Concourse. Explicit one-shot Commands
remain a separate execution contract.

## 2. Standard operator-pipeline contract

An operator instance is one operation over one InventoryCaptureGroup.

Example:
- Pipeline resource/external name: reconcile-ssh-host-keys-ssh-managed
- Job: operator
- Target group: InventoryCaptureGroup/ssh-managed
- Schedule: initially every 5 minutes
- Manual trigger: enabled

Use the standard Concourse time resource for interval scheduling. Calendar cron
expressions are not required initially. The job is serial so scheduled and
manual builds of the same operator cannot overlap.

The minimal inputs are:
- An explicitly configured target capture-group reference and API endpoint.
- Reviewed operator code in the existing Ansible repository or runner image.
- A versioned homelab runner image.
- Private API, SSH, and required OpenBao credentials.

One build performs one bounded reconciliation pass; it is not a daemon:
1. Read the capture group and current desired resources over authenticated HTTPS.
2. Resolve each target by kind, name, and UID; snapshot the relevant generation
   and dependencies for that target.
3. Inspect the real system through an authenticated channel.
4. Apply only necessary changes.
5. Verify the resulting system, including a fresh connection where applicable.
6. PATCH observed state to that resource's generic /status subresource.

Each later build rereads current state rather than replaying an old Command
snapshot. Empty groups are successful no-ops. Pending or failed targets do not
prevent examining other eligible targets. Unresolved references are reported,
not silently treated as convergence. A pass reports per-target outcomes and
fails its build when actionable targets fail.

Code must tolerate interruption and reruns. A successful Concourse build is not
itself proof that every target converged; per-target verified status is the
authoritative observation.

Start with one operator instance responsible for host keys on a Server. Serial
execution protects one job, not multiple pipelines selecting the same host.
Before any machine mutation, claim Server.status.operation using UID/If-Match
and a fresh ID, and release the same ID in finally. This excludes provisioning
reservations and other supported short operations across pipelines. A crashed
worker's claim requires inspection before release, not automatic expiry. See
RFC 0003's coordination contract; no lock service or lease resource is needed.

## 3. Resources and responsibility boundaries

| Resource/component | Responsibility |
| --- | --- |
| Server | Desired machine configuration and public observed identity/state. |
| Server controller | Create/own the host SSHKeyPair and expose its resolved public identity. |
| SSHKeyPair controller | Generate and maintain key material through its owned Secret. |
| Secret / SecretStore | Persist private material in OpenBao. |
| InventoryCaptureGroup | Select targets and project usable inventory; no SSH-specific schema fields. |
| Pipeline | Declare the persistent provider-specific operator pipeline. |
| Pipeline controller | Converge its configuration into Concourse. |
| Concourse operator job | Inspect, install, verify, and report machine-side state. |
| homelabd | Discovery and observations; no host-private-key delivery through the public agent token. |
| Command / CommandsPipeline | Explicit one-shot execution, independently of recurring operator builds. |

Initially declare operator Pipeline resources in homelab-init. Reuse the current
Pipeline spec: providerRef, externalName, and definition.format/data.
An operator task takes the capture-group name as a parameter; do not add an
operator-kind field to inventory resources.

Do not create an Operator, SSHHostKeyInstallation, or per-build resource merely
to represent this convention. Concourse owns operator scheduling/build history;
the API scheduler must not also create a Command for each timer tick.
Keep operator logic in reusable code, not large embedded pipeline scripts.
Later automation can generate these Pipeline resources if repeated declarations
justify it; no generic operator framework is needed to start.

## 4. Server-owned host-key lifecycle

For each Server lifetime:
- Create one stable Ed25519 SSHKeyPair using a UID-qualified child name and
  Secret path at the deployment-selected SecretStore.
- Record and validate ownership. Do not adopt an unrelated same-name key.
- Publish the child's name/UID, public key, fingerprint, and readiness in Server
  status. Private keys never appear in Server status, inventory, or Git.
- Wait for the child's current generation to resolve before installation.
- Keep the same identity across provisioning, reboots, and idempotent reruns.
- Recreating a Server with the same name creates a different owned identity.
- Key deletion/replacement must not silently authorize an identity change.
  Define explicit recovery/rotation approval before implementing that behavior.
- Server deletion cleans up only its owned child and corresponding Secret via
  existing finalizer patterns; public trust projections exclude deleted targets.

Expose desired public identity separately from the identity verified as installed.
The existence of a Ready SSHKeyPair does not mean a server is serving that key.

## 5. SSH host-key operator

For every selected Server:
1. Resolve its UID, generation, management address, desired host-key reference,
   and recorded first-seen bootstrap trust if it is not already managed.
2. Build a private per-run known_hosts file from recorded bootstrap and managed public identities.
3. Resolve only the required host private key through the privileged operator's
   secret-store access; keep it out of logs and public task outputs.
4. Connect as ansible using the runner's user certificate and strict host
   verification. Bind verification to the intended Server, not just its IP.
5. Stage the key with root ownership and 0600 private-key permissions; derive
   and compare its public key with the desired fingerprint.
6. Configure sshd to serve the intended identity, validate the configuration,
   install atomically, and reload without abandoning the recovery channel.
7. Establish a fresh connection that accepts only the desired managed key.
8. Report the verified installed key reference/fingerprint and observed Server
   generation through /status.
9. Persist the same key into the installed root before reboot, then verify it
   again after boot.

During a transition, permit only the durably recorded first-seen bootstrap key
and the desired managed key; do not accept arbitrary replacements. Keep a
recoverable transition for interruption between installation and verification.

Generate known_hosts from operator-recorded bootstrap pins and managed public
keys each run. Bootstrap pins retain the limitations of their TOFU origin:
storing a key in the API does not retrospectively authenticate the machine.
Use a Server-bound HostKeyAlias and check Server/key UIDs so ordinary managed
connections cannot be redirected to another trusted fleet member. Before the
first pin, attacker-controlled discovery/address data remains a risk.

Do not maintain a separate manually populated ansible-known-hosts Secret.
The runner must initialize its API/inventory context before constructing this
file, and must finish strict SSH initialization before invoking Ansible. Missing
trust remains a refusal to connect, not an empty file or verification bypass.

OpenBao grants for host-key installation are distinct from grants for user-CA
signing keys. The operator may need broad managed-host access initially, but it
must not receive CA private keys or unrelated secrets by default.

## 6. Status contract and drift

Report observations on the target resource, not through an operation-specific
endpoint. Initial implementation may use the existing broad runner status grant.

Server status.hostSSH observations:
- Desired managed key reference, including UID.
- Installed/verified managed key reference and public fingerprint.
- Server generation actually reconciled.
- A host-key readiness condition with a useful reason/message.
- An attempt/build identifier for diagnosis where useful.

Use UID checks and If-Match. On a conflict, reread and merge the operator's own
fields; preserve other status writers. Never mark a newer desired generation
complete based on an older run. Failure or cancellation must not report success.

Readiness changes only after verification. Periodic inspection must detect
missing/wrong keys even when spec has not changed; observedGeneration alone is
not evidence that runtime configuration is still correct. Avoid unconditional
timestamp writes and unstable condition ordering that create watch loops.

## 7. First-boot requirements

### Implementation locations

- Stigmergy internal/controller/server/host_keys.go: resource ownership,
  finalizer cleanup and public key projection, independently of machine binding.
- Server schema: internal/api/spec/resources/server.yaml; generated OpenAPI
  includes hostSSH status and the standard conditional /status route.
- ansible-roles/operators/common.py: reusable API, SSH and capture-group helpers.
- ansible-roles/operators/ssh_host_keys.py: bounded orchestration, TOFU pin
  persistence, owned-secret resolution, installation, verification and reporting.
- ansible-roles/operators/runner_trust.py: strict public trust for normal commands.
- ansible-roles/plays/ssh_host_keys.yml and roles/ssh_host_key: atomic key
  installation and validated sshd reload. plays/ssh_trust.yml reconciles user-CA
  trust after the managed identity is verified.
- arch-provisioner/profile.d/init.sh: operator credential initialization and
  normal-runner API-derived trust.
- homelab-init/Pipeline/pipeline-reconcile-ssh-host-keys.yaml: explicit persistent
  pipeline definition; upload.sh and the existing Pipeline controller handle
  creation/application. No separate generator or Operator API resource.
- ansible-roles/roles/init/files/openbao/: bootstrap the host-only AppRole and
  publish its stable credentials for Concourse. CA private access is excluded.

Controller-owned fields are keyPairRef/publicKey/fingerprint/keyReady.
Operator-owned observations are bootstrapPublicKey, installedKeyPairRef,
installedFingerprint, observedGeneration, phase/reason/message. Both preserve
other status writers and avoid unchanged writes.

Deployment must publish the changed runner and repositories, install policies
without rotating tokens, and apply the Pipeline and capture group. Live fleet
enrollment and installed-OS reboot continuity require real-machine verification.

### 7.1 Image and local account

The selected ISO must contain:
- The current public user-CA trust bundle and ansible principal policy.
- The fixed ansible account service/timer and intended privilege policy.
- homelabd and its restricted agent token.
- sshd plus boot-time host-key generation.
- A documented manual recovery procedure for an unreachable or mismatched host.

The public ISO must not contain a managed-server host private key, user-CA
private key, runner private key, or administrator token. Verify downloaded
artifacts against hashes obtained through the trusted API/build manifest.

The shared agent token may submit discovery claims and read Servers. Because
any ISO downloader can extract it, those claims do not establish machine
identity or authorize secret delivery. Keep host-private-key reads inaccessible
to that identity.

### 7.2 Select the intended machine

The administrator-created Server and operator target group determine which
machine may be bootstrapped. Resolve its Server UID and management address.
LLDP location, MAC/IP address, and MachineReports help discovery but are not
cryptographic proof. In particular, holders of the public agent token can forge
discovery claims. V1's TOFU policy accepts that first-contact risk; inventory
selection does not eliminate it.

### 7.3 V1 bootstrap: trust on first use

1. Boot the generic ISO; it generates a temporary, unique host key.
2. The privileged operator performs a deliberately scoped first-contact SSH
   probe and accepts one previously unknown Ed25519 host key.
3. Persist that public key as the first-seen bootstrap pin, tied to the Server
   UID, before delivering a private key or performing privileged changes.
4. Subsequent connections, including retries and other builds, use that pin
   with strict checking until the managed-key transition completes.
5. Install the controller-owned managed key and reconnect accepting only that
   managed public key.
6. Report the verified transition; stop accepting the temporary bootstrap key.

OpenSSH's StrictHostKeyChecking=accept-new is a suitable first-contact primitive:
it accepts previously unknown keys but rejects changed keys. It must not be
used with a fresh empty known_hosts file on every operator pass. Rehydrate the
recorded pin each time, and require successful UID-qualified persistence before
any key installation. A lost/conflicting pin write must not proceed.

An SSH scan/probe and a later connection must agree on the pinned public key;
do not scan, discard the result, and connect with blanket checking disabled.
Disable automatic unrelated host-key learning and agent forwarding.

Only the privileged operator records bootstrap observations. The public agent
token cannot approve enrollment or read host private keys. No extra manually
created SSHKeyPair resource or console integration is required.

This is not protection against first-contact impersonation. An attacker who
controls the network or redirects discovery during enrollment can be pinned
as the intended host, receive its managed private key, and continue
impersonating it after installation. Verifying the managed key afterward does
not undo that compromise. V1 therefore assumes the initial management network
and discovered target are trustworthy; do not claim zero root-escalation risk.

Normal commands/provisioning never get a host-verification bypass. The TOFU
exception belongs only to the initial enrollment operation. It is not an
automatic fallback after an established host fails verification.

### 7.4 Transition, persistence, and reenrollment

The intended progression is:

Boot/discover -> record first-seen bootstrap key -> resolve managed key
-> install in live system -> verify managed SSH
-> install/persist to disk -> reboot -> verify installed system

Only a fresh managed-key verification advances installed readiness. Report
bootstrap-in-progress separately from managed readiness. Missing managed
material reports ManagedHostKeyPending. Authentication failures, unexpected
identity changes, and ambiguous bindings stop changes.

Rebooting the installed system preserves the managed key. Booting fresh generic
media does not inherit trust from an old IP or a reused Server name. An existing
Server with a different host key must require explicit administrator reenrollment,
not automatically restart TOFU. This distinguishes a planned fresh ISO boot from
unexpected identity changes. A new Server UID is a new lifetime; selection for
bootstrap must still be administrator-controlled.

If no accepted host identity is available, the SSH operator cannot remotely
repair missing/wrong host keys through that same SSH channel. It reports the
failure and requires manual recovery or explicit reenrollment.

## 8. V1 decisions and deferred work

- TOFU is accepted only for a fresh, explicitly selected Server lifetime.
- Planned fresh-media reenrollment requires pausing the operator and an
  administrator's conditional /status reset of bootstrapPublicKey,
  installedKeyPairRef and installedFingerprint, then resuming it. Retain the
  controller-owned identity. Broad trusted status permissions remain v1 policy.
- Generic inventory and Pipeline schemas are unchanged.
- The time resource schedules every five minutes; the task has a 15-minute
  timeout, SSH probes a 45-second timeout, and each Ansible invocation five minutes.
- Deliberate host-key rotation is deferred; missing/replaced established
  identities fail closed. Server deletion cleans its owned key/Secret.
- Real fleet boot/reboot verification and overlapping user-CA rotation remain
  rollout checks. No automatic KVM or console recovery is part of v1.

## 9. Verification criteria

- Creating one Server produces exactly one owned key pair without a second manifest.
- Reconciliation/restarts preserve its identity; same-name unrelated children
  are rejected and Server-name reuse cannot inherit a prior lifetime's keys.
- Private material is absent from status, inventory, Git, logs, and public images.
- The agent token cannot read host private keys or approve bootstrap trust.
- Scheduled and manual operator builds run through the same serial job.
- Empty groups are no-ops; one blocked target does not prevent other checks.
- Only selected initial-bootstrap targets can enter TOFU. Established or
  bootstrap-pinned identity mismatches receive no keys or disk modifications.
- After the scoped TOFU probe, SSH remains strict and verifies the pinned or
  managed identity for that Server, not any trusted fleet key.
- Interrupted installation recovers safely; success requires a fresh verified
  connection and verification after installed-system reboot.
- Missing/wrong runtime keys are detected without a desired-generation change.
- Concurrent status updates preserve unrelated fields and produce no write loop.

## References

- [Kubernetes controller pattern](https://kubernetes.io/docs/concepts/architecture/controller/)
- [Concourse time-triggered jobs](https://concourse-ci.org/examples/time-triggered/)
- [Concourse job serialization](https://concourse-ci.org/docs/jobs/)
- [Concourse time resource](https://github.com/concourse/time-resource)
- [OpenSSH StrictHostKeyChecking](https://man.openbsd.org/ssh_config#StrictHostKeyChecking)
