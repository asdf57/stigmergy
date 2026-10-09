# RFC 0003: Provisioning runs and network boot

- Status: Desired design agreed; shared fresh-live-boot lifecycle requires hardware acceptance
- Updated: 2026-10-08
- Related: RFC 0001 (ISO/SSH management), RFC 0002 (external operators)

## Decision

Server describes the desired host, Machine describes discovered hardware, and
ProvisioningRun is an immutable one-shot installation request. Disk choices
belong to the run, never to Server spec. Enabling provisioning, changing an OS,
publishing an ISO, or rotating a CA does not authorize erasure. Creating a run
explicitly authorizes replacement of its selected disk, including existing data.

The API stores and validates resources and renders boot instructions. External
operators execute reviewed Ansible stages and report through generic object
status. homelabd discovers/enrolls hardware; it never provisions disks. There
is no per-run pipeline, special action endpoint, or generic workflow engine.

## Desired provisioning lifecycle (design boundary)

Every new ProvisioningRun uses this same path, regardless of the current OS,
current live ISO, previous success, or point of failure:

```text
Confirm Server and disk -> pin inputs -> prepare fresh live boot
  -> GRUB/iPXE (installed) or kexec (live) -> verify fresh live boot
  -> wipe only approved disk -> install -> boot locally -> verify
```

1. A new request is an explicit start-from-scratch authorization. Delete a
   Blocked request to release ownership, then create an ordinary new request;
   no replacement field or special retry provisioning path is needed.
2. Before reboot, verify ownership, Server/Machine/disk identity, boot capability
   and artifact availability. A mounted installed root, staging mounts, or swap
   from the previous attempt must not be mistaken for a reason to skip boot or
   demand that the current environment already be installation-ready.
3. Always prepare and execute a fresh live boot through Ansible. Installed hosts
   use GRUB/iPXE; live hosts use generic kexec with orderly systemd shutdown.
   Initial firmware network boot uses iPXE. Already running the desired live
   image is not an exemption: require a changed boot ID and the pinned build.
   Never reuse the source session. Unsupported/failed handoffs stop safely.
4. ISO bootArguments and artifacts encapsulate image-specific kernel/initrd/rootfs
   details. Both iPXE and kexec consume that shared recipe, pinned in the run.
   All supported ISOs implement the same boot and agent readiness
   contract; the provisioning state machine does not grow distro-specific
   recovery flows. Distro-specific installation roles are still appropriate.
5. Only in that verified fresh live environment check the target is independent
   of the running root, unmounted, and not active swap; then erase and install.
   Previous mounts and swap normally disappear with reboot. Unexpected state
   after fresh boot is a safety failure, not permission to force cleanup.
6. Complete only after a fresh installed boot verifies the requested OS,
   approved disk, run marker, management identity and healthy agent/discovery.
   Normal non-provisioning boots stay local; only an explicit run requests netboot.

Recovery restores this ordinary lifecycle; it does not become a second installer.
Do not create temporary GRUB partitions just to leave live mode: kexec does not
require persistent boot storage. Installed GRUB remains local-by-default with
an iPXE entry. Neither GRUB nor kexec guarantees recovery from power loss or an
unreachable host. Kexec does not perform firmware hardware reinitialization;
test NIC/platform support explicitly. A failed handoff never permits erasure. If the node is unreachable or its boot path is
unusable, stop and request scoped manual console/live-media recovery. Never
claim arbitrary power-loss recovery or use PiKVM.

Fix the lifecycle invariant, not just the latest error. A one-off recovery action
may restore access, but is not a permanent fix or acceptance evidence. Acceptance
must cover both Arch and Debian, starting from installed and live sessions,
including the already-current live build, OS changes, and fresh requests after
failures before erasure, during partitioning, during configuration, and during
final boot. Demonstrate the fresh boot and selected-disk boundary in each case.
Tests must also prove that no new request means no automatic replay of erasure.

Implementation status: shared ISO bootArguments and the generic live kexec path
are implemented locally; deployment and hardware acceptance are separate checks.
No matching-image live-session shortcut or pre-reboot staging-cleanup installer
path is part of this design.

## Resource contracts

Server spec contains machineSelector, operatingSystem, boot.isoRef, SSH CA,
users/packages/settings, provisioning.enabled and reconciliation.paused.
Server status contains machineRef, management/SSH readiness and:

```yaml
provisioning:
  provisioned: true
  maintenance: false
  lastRunRef: {name: latest-install, uid: <run-uid>}
  lastSuccessfulRunRef: {name: completed-install, uid: <run-uid>}
  # activeRunRef exists only while a run owns the Server.
```

Machine.status.inventory.storage is the discovered disk inventory. A device ID
is wwn:<lowercase-wwn>, or serial:<serial> when WWN is unavailable. Paths such as
/dev/sda are observations, never durable authorization. No extra Disk resource
is necessary for v1.

```yaml
apiVersion: homelab.io/v1alpha1
kind: ProvisioningRun
metadata:
  name: beelink-install-001
spec:
  serverRef: {name: beelink, uid: <server-uid>}
  serverGeneration: <reviewed-server-generation>
  machineRef: {name: machine-c16d3cb92fe4, uid: <machine-uid>}
  storage:
    disks:
      - deviceID: wwn:0x53a5a277260208cc
        role: system
```

References require names and UIDs. The array has exactly one system disk in v1.
Multiple disks, RAID, encryption and extra storage roles require explicit layout
semantics and acceptance tests; selecting several disks must never implicitly
mean erase everything. Existing capture-group variables carry the reviewed
EFI/swap/ext4 layout and common installation inputs, not disk-selection authority.

Creation checks the reviewed Server generation and reciprocal Machine binding,
requires established managed SSH identity, and checks enabled/unpaused
state, rejects an existing reservation, and resolves exactly one discovered
SATA/ATA/NVMe physical disk. It records serial, WWN, size, model and transport in
status.selectedDisk. USB and ambiguous/unidentified targets are rejected.
Discovery is not attestation: the operator rechecks real hardware, removability,
mounts and boot identity on the node before changing anything.

An etcd transaction creates the run and sets Server maintenance/activeRunRef
with a Server resource-version comparison. Two concurrent creations against the
same Server cannot both succeed, even with different run names. Unrelated status
updates may cause a safe conflict; clients refresh and reconfirm, never retry
destructive intent blindly. Specs are immutable from creation.

## Discovery and disk-selection UI

1. An unknown MAC boots the configured discovery ISO and homelabd reports the
   hardware/LLDP location. The binding controller resolves Machine to Server.
2. A new, bound Server with provisioning enabled, boot ISO and target OS may
   receive its configured Ready live ISO, without a run or disk choice.
3. The SSH operator establishes management access. Booting live or obtaining SSH
   never authorizes disk erasure.
4. Provision opens a dialog showing disks from the UID-bound Machine inventory:
   model, capacity, serial/WWN and transport. Unsupported devices are unavailable.
   The current inventory does not report per-disk partitions/mounts; do not
   pretend it does. The execution probe checks those on the node.
5. The owner selects one system disk and types the exact Server name to confirm
   permanent replacement. In the same dialog, distribution/version/live ISO
   choices are derived from current Ready ISO resources with completed builds,
   filtered to supported amd64 UEFI targets (Arch rolling and Debian trixie).
   Multiple matching images require an explicit choice. On confirmation the UI
   conditionally saves the selected OS and UID-qualified boot ISO in Server spec,
   preserving other settings, then POSTs a new ProvisioningRun using the returned
   generation. Selecting options alone writes nothing. Saving spec and creating
   a run are separate operations: a failed request may leave desired OS/ISO saved
   without a confirmed run. Require refresh/review; never silently retry or undo
   another writer's state. Installer preflight remains the authority on readiness.
6. The shared provisioning job picks it up on its next scheduled/manual pass.

New-machine discovery does not forcibly reboot arbitrary existing installations.
Normal installed boot remains local. The UI must not test confirmation against a
real disk; mocked requests are the safety regression boundary.

## Operator and checkpoints

```text
Pending -> PreparingBoot -> AwaitingLive -> Installing
        -> AwaitingInstalled -> Verifying -> Succeeded
        failure -> Blocked
```

All checkpoints, messages, execution identity, timestamps, boot IDs, bootstrap
pin and immutable snapshot belong to ProvisioningRun.status. Its UID is the
attempt ID. The root-owned installed marker records Server UID, run UID, plan
digest, root UUID, EFI partition UUID and loader.

The execution snapshot pins Server inputs, Machine/key/ISO/CA UIDs, trust bundle,
immutable ISO build/artifacts, selected disk identity, resolved stable by-id path,
boot MAC, inventory layout and reviewed operator Git revision. It contains no
private key, API token or daemon enrollment credentials.

The operator lists captured Servers and follows their activeRunRef. It resolves
inventory through Ansible, not a flat hostvars map. A Partial capture group may
still contain usable targets. Each bounded pass processes at most one run.
Pending runs on other Servers do not deadlock each other; another already-started
run retaining maintenance blocks a new claim.

Preflight checks current strict SSH, supported amd64 UEFI OS, Secure Boot already
off, identities, disk safety, layout, immutable image and dependencies. The remote
probe matches serial/WWN/size/model/transport and resolves a verified by-id alias.
Missing/ambiguous/USB/removable/read-only targets block. Initial and repeat installs
both require an explicit run; there is no automatic blank-disk installation.

After claiming PreparingBoot, every privileged stage rechecks run ownership and
snapshot inputs. It primes the boot NIC, checkpoints live intent, arms/readbacks
the GRUB/iPXE selection for installed hosts or loads the pinned kernel/initrd
for live kexec, then transitions through Ansible. Both paths require a fresh boot. Follow the desired lifecycle above; never erase under an unverified build.

AwaitingLive requires the changed boot ID and pinned ISO build. The v1 live-key
handoff uses scoped TOFU for this attempt only, with its acknowledged impersonation
risk. It records the bootstrap pin before delivering a managed private key, keeps
both keys during sshd reload, then strictly verifies the managed identity. It
does not globally disable host checking or reenroll ordinary key mismatches.

Installing is persisted before erasure. The Ansible role independently rechecks
the complete management setup bundle before disk operations. Image CI verifies
the retained bundle inside both ISO and PXE artifacts before publication; the
shared installer copies complete reviewed asset directories across handoffs.
Missing installer dependencies must fail before erasure. The role then rechecks
the exact disk, boot ID/build, unmounted target and protected enrollment, installs
the OS from Server inputs (not the live distro), and preserves the managed key,
CA bundle, fixed ansible account timer/service and homelabd token.

AwaitingInstalled/Verifying checks a fresh installed boot, exact root/disk/marker,
expected OS, restored GRUB and healthy management services. Final Ansible checks
also query LLDP as homelabd and require a neighbor. Both images and installed
systems include lldpd; the shared service PATH includes /usr/sbin. Debian uses
the _lldpd socket group and a persistent root:_lldpd 0750 client override instead
of setuid/adm access. Apply the shared setup after ISO package installation and
during installed-agent setup. Only then mark the
run Succeeded and release the Server reservation, recording lastSuccessfulRunRef.
Run completion is durable before release. A crash between these writes resumes
release, not installation. Failed runs never replace the last verified success.

Server detail polls generic Server and ProvisioningRun GETs every five seconds
while visible. It displays workflow checkpoints and build/message data, not an
invented event history. Stale/replaced-UID poll results are rejected.

## Boot contract

Normal: firmware -> disk GRUB -> installed OS.
Requested replacement: disk GRUB -> local iPXE -> API -> pinned live ISO.
First discovery: manually booted live/PXE -> enrollment and managed SSH.
First installation: explicit run -> fresh pinned live boot -> operator.
Live-to-live: shared image recipe -> orderly kexec -> fresh pinned live -> operator.
Final boot: newly installed disk GRUB -> installed OS.

GRUB defaults locally to the installed OS, uses a visible five-second menu,
and consumes grub-reboot homelab-netboot once. Verify grubenv readback before
reboot and clear next_entry after installation. Do not make network/API
availability a dependency of normal boots.

The embedded-script iPXE artifact trusts packaged ISRG X1 and USERTrust ECC/RSA
roots. TLS stays enabled. Permission denied may mean TLS validation, not HTTP
auth. Test the actual served chains with the UEFI VM fixture. Publication alone
does not refresh an installed /boot/ipxe/ipxe.efi; use guarded boot-only repair.

An active run serves its pinned immutable image during PreparingBoot,
AwaitingLive and Installing. Pending may serve the configured Ready ISO.
Blocked, final installed-verification, stale/missing run and binding/dependency
replacement produce non-installing boot errors. No claimed run means only a
not-yet-provisioned Server can receive its configured discovery/live image.

Arch netboot uses ip=dhcp net.ifnames=0 BOOTIF=01-<verified-MAC>. Without BOOTIF,
ip-config can fail with SIOCGIFFLAGS. The current image needs at least 4 GiB for
tested RAM-live boot; 2 GiB exhausted RAM in the fixture.

Debian fetch boot uses BOOTIF without ip=dhcp: live-boot interprets that override
as static configuration and writes an invalid nameserver dhcp. Fetch obtains
DHCP automatically. An affected existing live session can restore validated
lease DNS through the guarded Ansible repair_debian_live_dns.yml playbook;
cleanup/release and a new approved run remain required after a pre-erasure failure.

Resolve the pinned NIC MAC against current interfaces before every priming stage;
live eth0 and installed enp1s0 differ. Require a unique match, recheck MAC, enable
magic-packet WoL and disable EEE only for the configured affected NIC, then read
back. No blanket ignored failures or sysrq-reset fallback.

UEFI reconciliation matches the exact new EFI PARTUUID, active Homelab label and
loader /EFI/Homelab/grubx64.efi. grub-install may retain a stale same-label entry.
Register/select the exact entry while preserving other firmware entries. Never
write arbitrary vendor BIOS variables or assume a programmatic BIOS setup API.

## Coordination and recovery

The SSH and provision jobs share server-lifecycle in the same persistent
reconcile-ssh-host-keys-ssh-managed pipeline. Server maintenance gates the SSH
operator and normal runners. Any reservation pauses API Command dispatch;
already-dispatched Commands must drain before reboot/erase. This is not a lock
on administrator SSH or directly started external jobs. Use only the supported
worker path; do not run multiple provisioning workers.

Pause/disable prevents new stages, not proof of cancellation. A completed install
may finish its safe final boot before paused post-configuration resumes.
Preparation failure clears and verifies owned boot selection where possible.
Uncertain cleanup retains maintenance. Blocked with maintenance requires inspection.

For a proven failure before erasure, explicit Ansible cleanup verifies the old
installed marker/root on the selected disk and clears any lingering GRUB
next_entry. A false netbootArmed status is not proof of physical cleanup. After
unmounting and verification, retain the failed run, clear its maintenance and
release the Server reservation. The UI distinguishes this historical failure
from an active blocked request; a fresh installation still requires confirmation.

An interrupted Installing run never reexecutes erasure automatically. Explicit
configuration-only repair verifies the original live session/build/disk, staged
root/EFI/bind mounts and marker, then completes configuration without partitioning,
formatting or bootstrapping again. It may advance that same Blocked run to
AwaitingInstalled, retaining ownership and the original snapshot. It cannot return
to Installing. Fresh installed verification remains mandatory.

AwaitingInstalled/Verifying can resume with the original pinned code revision.
Lost completion only reconciles terminal status/reservation. A new destructive
attempt requires a new explicitly confirmed run. A Blocked run can be deleted
with If-Match even while reserved: an etcd transaction deletes it and clears only
its UID-matching Server references and maintenance reservation. Active phases
cannot be deleted; collection deletion is rejected. Deletion loses run history
but does not change the node, boot selection or disk. No replacement field is
needed: after deletion create an ordinary new run with fresh confirmations.
The new run uses GRUB/iPXE or live kexec to enter a fresh pinned live environment
before installation readiness checks. It does not depend on repairing the old
session's staging mounts or swap to reuse that session. Unexpected mounts/swap
in the fresh environment block erasure; never force or lazy unmount to pass checks.
Retained runs provide separate results; no TTL or automatic deletion is implemented.
Pending cancellation can explicitly report Blocked with maintenance
false only before any privileged work, then release the Server reservation.

A destroyed GRUB/disk or power failure during replacement may require manual
live-media recovery. Firmware PXE-first or a permanent USB can bootstrap initial
discovery, but neither automatic firmware recovery nor a protected bootstrap
partition is promised. Never use PiKVM. Manual recovery does not authorize erasure.
Disable operator execution during API/Concourse snapshot restore and inspect
markers/reservations before reenabling; no exactly-once erase guarantee is claimed.

## Ownership and verification

- stigmergy/internal/api/spec/resources/provisioning-run.yaml: generated generic
  CRUD/status/OpenAPI contract; Server schema contains only the lightweight gate.
- stigmergy/internal/api/provisioning.go: creation validation, atomic reservation,
  checkpoint validation and guarded release. internal/store/etcd implements the
  generic create-with-related-status transaction, not provisioning execution.
- stigmergy/internal/api/ipxe.go: discovery and UID/run-pinned boot rendering.
- ansible-roles/operators/provisioning.py: bounded external orchestration;
  plays/provision_stage.yml and provision/grub/management roles own mutations.
- stigmergy-web: disk selector and POST; run-backed checkpoint polling.
- homelabd/utils/stigmergy_types.go: matching lightweight Server API types;
  the agent does not consume or execute ProvisioningRuns.
- homelab-init: persistent shared pipeline/capture group and Server configuration,
  never a checked-in destructive run automatically replayed by init.

Runner credentials read public dependencies/runs and PATCH generic Server/run
status; they cannot create provisioning requests or fetch API Secrets. Admin
creates runs. Preserve existing API tokens and Git SSH keys when preparing policy.
Broad trusted status credentials are accepted in v1; validation is not physical
attestation or a fine-grained authorization system.

Verify schema/generated routes, immutable spec, UID replacement, simultaneous
creation/CAS, unsupported/ambiguous disks, boot discovery without a run, pinned
image stability, pause/cleanup, interrupted Installing, lost completion/release,
and UI confirmation/conflict handling. Format operators with YAPF and run Pylint
without hiding existing warnings. Run Ansible syntax checks and the UEFI boot/TLS
fixtures. Unit/green build results do not substitute for a complete installed ->
live -> installed hardware acceptance run.

## Hardware acceptance context

The 2026-10-07 rollout deployed the API and web console, ran homelabc init,
preserved API tokens and SSH keys, and verified both shared lifecycle jobs using
the pushed operator revision. Both live ISOs rebuilt with the current homelabd
revision and their public download URLs returned HTTP 200. Strict read-only SSH
verified Beelink's installed Arch root on its approved SSD and its GRUB contract.
Rollout tooling created no installation requests. The first owner-created run
booted into its pinned live image but failed an installation assertion before
erasure. The role now validates the run UID directly; real Ansible assertion
tests cover existing partitions, wrong run/session/disk and absent authorization.
That failed run was explicitly cleaned up without erasure or reboot: the prior
root/marker were verified, a lingering GRUB next_entry was cleared and the
reservation released. The failed run remains history, not an active lock. A
fresh replacement still requires approval to erase/reinstall that SSD. The desktop
Server remains unbound; capture groups report its omission rather than pretending
it has management access.

Beelink EQ13: management MAC e8:ff:1e:d4:03:fa; approved SATA SSD serial
MP23B72602251, WWN 0x53a5a277260208cc, 512110190592 bytes, stable alias
/dev/disk/by-id/ata-512GB_SSD_MP23B72602251. A separate 14.9 GiB USB is excluded.
These are point-in-time observations, never authorization for another run.

The prior physical flow verified GRUB/iPXE live transition, installed ext4 root,
strict SSH continuity, managed services and corrected UEFI partition selection.
The latest read-only API checkpoint before this source change reported a completed
installation with maintenance cleared. No provisioning request, reboot or erase
was issued while implementing this resource/UI refactor. New-model deployment
and physical acceptance remain distinct from existing-model success.
