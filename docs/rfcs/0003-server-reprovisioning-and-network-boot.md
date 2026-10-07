# RFC 0003: Server reprovisioning and network-boot orchestration

- Status: Draft; no provisioning implementation or hardware changes authorized by this RFC
- Created: 2026-10-06
- Scope: initial provisioning, explicit reprovision requests, API state, external execution,
  GRUB/iPXE boot selection, and the installed/live SSH transition
- Related: [RFC 0001](0001-iso-pipelines-and-ssh-management-access.md),
  [RFC 0002](0002-external-operators-and-first-boot-ssh-trust.md)

## 1. Decision and boundaries

Use Server desired state plus a monotonic request counter, following the
generation/observed-generation pattern without using ordinary Server generation
as permission to erase a disk. First installation and later intentional
reinstallation are distinct cases: enabling initial provisioning with a complete
OS/disk configuration does not require incrementing the reprovision counter.

The API validates and stores desired/observed state and renders boot instructions.
One persistent Concourse operator executes the existing Ansible provisioning
code. homelabd reports discovery; it does not provision disks or certify success.

Firmware normally boots disk GRUB. GRUB defaults to the installed OS and has one
stable Homelab netboot entry that launches iPXE. For an authorized reprovision,
the operator selects that entry for one boot, then reboots into the API-selected
live ISO. Initial installation may start from an already-running live ISO, as
Beelink does today; firmware PXE is not a prerequisite for that path.

Normal installed boots do not depend on the API or network. A permanent boot USB,
firmware PXE-first setup, protected bootstrap partition, and automatic recovery
from a destroyed bootloader are not v1 requirements. The owner accepts manually
repairing/booting a live image if GRUB, its disk, or the OS becomes unusable.

Do not add a ProvisioningRequest, per-attempt Pipeline, automatic Command, or
generic workflow engine in v1. Concourse retains execution logs; Server status
retains the active/last attempt and last verified success. Commands remain
independent one-off administrative executions.

This document is a plan, not authorization to wipe Beelink or reboot any host.
Provisioning intentionally has root-equivalent power. Broad trusted operator
status writes are acceptable initially; fine-grained permissions are later work.

## 2. Existing implementation reviewed

The basis is ansible-roles/plays/provision.yml:

1. Optional API/infrastructure setup.
2. Detect /var/lib/is_live_env; reboot into iPXE if needed.
3. Check that the restricted daemon enrollment exists.
4. Run prereqs, optional wipe, partitioning, and bootstrap.
5. Reconnect and run post-provision configuration.

Relevant existing files:

- roles/provision/tasks/reboot.yml: prime NIC, grub-reboot, then reboot.
- roles/provision/tasks/prime_nic.yml: enable WoL and disable EEE.
- roles/grub/tasks/main.yml and reboot_to_ipxe.yml: install/select iPXE.
- roles/provision/tasks/bootstrap.yml: GRUB/iPXE installation and final reboot.
- roles/provision/tasks/await_and_refresh.yml: wait for port 22 and reset SSH.
- roles/management/tasks/main.yml: install fixed management access
  and preserve the managed SSH identity in the installed root.
- roles/init/templates/dnsmasq.conf.j2 and files/nginx/boot.ipxe: PXE entry path.
- stigmergy/internal/api/ipxe.go: MAC -> Machine -> Server -> ISO resolution.

Current Server schemas already have spec.provisioning.enabled,
spec.reconciliation.paused, and status.provisioning attempt/stage fields.
These are useful scaffolding, not an implemented provisioning controller.
Reuse and clarify them rather than building a parallel status model.

Gaps that must be closed:

- Reboot/GRUB/NIC errors are currently broadly ignored.
- Port 22 opening does not prove the intended host, boot session, or environment.
- /ipxe/<mac> currently serves a configured live ISO without attempt pinning.
  It must distinguish discovery from an active attempt and refuse stale or
  unresolved attempt state. It does not need to choose the installed OS on every
  normal boot: GRUB already does that locally.
- Existing GRUB snippets disagree on names/IDs and chainloading. Passing iPXE
  commands as GRUB chainloader arguments is not a validated substitute for
  an embedded iPXE script or DHCP-delivered script.
- Existing boot-priority services must be reviewed so they do not bypass disk
  GRUB or alter its deliberately armed one-shot netboot selection.
- The SSH operator must not treat the deliberate live-image key change as an
  ordinary managed connection, nor race the provisioning operator.
- InventoryCaptureGroup/servers can be Partial because Desktop is undiscovered.
  A ready Beelink must not be prevented from provisioning by that dependency.

## 3. Minimal Server API contract

Proposed desired fields, extending the existing provisioning object:

```yaml
spec:
  provisioning:
    enabled: true
    reprovision: 0 # Initial installation; increment later to request reinstallation.
    targetDisk: /dev/disk/by-id/<verified-Beelink-disk-id>
  operatingSystem:
    distribution: arch
    version: rolling
    architecture: amd64
    bootMode: uefi
  reconciliation:
    paused: false
  boot:
    isoRef: {name: arch-rolling-amd64}
```

targetDisk must identify exactly one inspected physical disk by stable identity;
it is not a default /dev/sda or a best-effort first-disk selection. The partition
layout and other playbook inputs can initially come from existing inventory
groupVars. Do not introduce a GroupVars API resource or inventory-specific
provisioning schema. Snapshot the resolved non-secret execution inputs before
starting; inventory is configuration transport, not authority to choose a disk.

Proposed status, extending existing attempt fields:

```yaml
status:
  provisioning:
    provisioned: false
    observedReprovision: 0
    phase: PreparingBoot
    attemptID: <unique-attempt-id>
    requestedReprovision: 1
    observedServerGeneration: 12
    currentStage: PreparingBoot
    backendRunID: <Concourse-build-id>
    bootTarget: live # Attempt intent, not the routing policy for every normal boot.
    startedAt: <timestamp>
```

Retain useful existing timestamps/message fields. Add a bounded execution
snapshot for Server/Machine/key/ISO UIDs, exact immutable ISO build, target disk
identity, provisioning inputs and reviewed code revision. Store no private keys
or agent/client tokens in it. Add boot/session observations needed for recovery.
The snapshot describes what this attempt will install; it is not a new resource.

### Initial provisioning versus reprovisioning

| Case | Eligibility and behavior |
| --- | --- |
| First installation | Enabled, unpaused, complete desired OS/disk/layout, bound Machine, and no verified installation/previous unsafe attempt. Counter 0 is sufficient. Boot the selected live ISO and install the OS specified in Server spec. |
| Already provisioned | Matching installed marker and verified status: no reinstall merely because a Server spec or ISO build changes. |
| Intentional reprovision | Increment reprovision above observedReprovision. Arm live boot, reboot the installed OS, reinstall the requested OS, verify installed boot, then advance the observation. |
| Already live | Enter installation only for an eligible initial or explicit reprovision attempt; do not reboot merely to reenter the same live environment. |
| Lost status or recreated Server | Inspect identity and installed marker; recover observations or block. Missing provisioned status is not proof of a blank/new machine. |

Initial success records provisioned: true and observedReprovision: 0. Those
fields together distinguish a successful initial installation from "nothing
has happened." Persist the initial attempt/checkpoints too: a failed or
interrupted counter-0 installation must not restart destructively every tick.

An existing installation, existing target-disk partitions of unknown ownership,
or a marker belonging to another Server UID blocks automatic initial erasure.
Inspect/adopt verified existing state separately, or deliberately authorize
replacement with the explicit reprovision path. Do not adopt an arbitrary OS
merely because it is reachable. Enabled initial provisioning plus complete
target configuration authorizes initial installation on the approved fresh disk,
not erasure of whichever disk the playbook finds first.

The installation OS comes from spec.operatingSystem. The boot ISO is an
execution environment, not the desired installed OS. Refactor existing role
branches that choose installation recipes from the live host's ansible_facts.
Validate supported live/target OS combinations rather than silently installing
the live distro when it differs from the Server's requested distro.

Counter semantics:

- Missing reprovision means 0. Zero permits the eligible first installation;
  it does not request a repeat installation of an already provisioned Server.
- Clients request intentional reinstallation by incrementing the counter with
  the existing conditional Server PATCH/PUT and If-Match.
- Enforce nonnegative bounded int64 and monotonicity in API update validation,
  not only in OpenAPI. Normal increments are +1; reject decreases and jumps.
- After initial provisioning, only a newer counter authorizes reinstallation.
  Changes to labels, packages, CA trust, ISO publication, or ordinary generation
  do not authorize another destructive attempt.
- Disabling provisioning or pausing prevents new attempts; neither means success.
- observedReprovision is the last fully verified successful request, never the
  last request read, claimed, dispatched, or failed.
- A failed request does not advance observedReprovision. Do not automatically
  start another destructive attempt merely because requested > observed.
- Once an attempt starts, reject another increment and changes to its destructive
  execution inputs until the running build is finished and the attempt terminal.
  Compare relevant inputs, not unrelated Server labels or all global generation.
- After terminal failure, a new increment explicitly requests a fresh attempt.
  Old failures remain in Concourse history; skipped failed counters are not
  retrospectively reported as successes.
- Same-name Server replacement is a different lifetime and starts at 0.
  Never copy authorization or attempt state to a different Server/Machine UID.

Pause/cancellation is cooperative at stage boundaries. It cannot undo an erase.
A paused operator must not abandon a host between an unsafe half-written state
and a safe checkpoint; report the actual stage and stop before the next action.

## 4. Resources and code ownership

Extend these existing instances in homelab-init, using existing resource kinds:

| Instance/component | Responsibility |
| --- | --- |
| InventoryCaptureGroup/ssh-managed | Project available managed Server inventory for both lifecycle jobs. |
| Pipeline/reconcile-ssh-host-keys-ssh-managed | SSH job and provisioning job in the same shared serial group, with timed/manual triggers. |
| Server/beelink | Bind the machine, choose the ISO and exact disk, request an attempt. |
| Existing ISO, CA, SSHKeyPair, SecretStore, PipelineProvider | Resolve boot artifacts and management identities; no duplicate resources. |

Use the existing homelab.io/ssh-management: enabled selection, plus
spec.provisioning.enabled and either eligible first installation or a newer
explicit reprovision request. No additional label
or inventory group is necessary initially. A Concourse manual trigger
only polls desired state; it never increments the counter or authorizes a wipe.

Code placement:

- stigmergy/internal/api/spec/resources/server.yaml: request/status schemas and
  regenerated API/OpenAPI/client types.
- stigmergy/internal/api: monotonic/active-attempt validation using current stored
  state and conditional updates; no custom reprovision action endpoint.
- stigmergy/internal/api/ipxe.go: discovery and attempt-pinned live boot routing;
  no installed-loader chainloading or routine installed-boot dependency on the API.
- Existing Server controllers: dependency readiness and identity relationships,
  preserving operator-owned provisioning status.
- ansible-roles/operators/provisioning.py: bounded orchestration, attempt claiming,
  stage validation, SSH transitions, recovery and generic /status reporting.
- ansible-roles/plays/provision.yml and roles/provision: reusable installation
  stages, not a second provisioning implementation in Python.
- ansible-roles/roles/grub: one validated stable iPXE entry, installed-OS default,
  and tested one-shot GRUB environment handling.
- Existing prime_nic.yml: the reviewed hardware workaround.
- homelab-init/Pipeline: shared lifecycle job declarations; reuse ssh-managed.
- homelabd: boot/session observations only if existing reports lack them.
- arch-provisioner: shared runner primitives only if required; no host-private-key
  delivery in ordinary command initialization.

The operator handles eligible Servers independently, like the SSH operator.
A Partial group does not authorize work on omitted targets, but does not block
ready captured targets. Bound the pass and process at most one destructive
attempt at a time in the single v1 job. Scheduled idle passes are safe no-ops.
A provisioning operator may run longer than the SSH operator; configure stage
and overall timeouts for real installation durations rather than inheriting 15m.

## 5. Boot design: local GRUB default, one-shot netboot

```text
Normal boot:       firmware -> disk GRUB -> installed OS
Reprovision boot:  firmware -> disk GRUB -> iPXE -> API -> pinned live ISO
Initial install:   existing live session OR manually booted ISO/PXE -> operator
Final reboot:      newly installed disk GRUB -> installed OS
```

### 5.1 Installed boot contract

Install disk GRUB in the existing UEFI provisioning path, with:

- The installed OS as the persistent default.
- One stable menu-entry ID, homelab-netboot, displayed as Homelab netboot.
- A locally installed, reviewed UEFI iPXE binary containing the common bootstrap
  script. The GRUB entry chainloads that binary; do not depend on unverified
  chainloader argument handling to deliver the script.
- A known GRUB environment-block location and a configuration that honors and
  consumes next_entry while keeping the installed OS as the normal default.
- A visible, bounded menu timeout for manual intervention.
  Explicitly generate menu style with a five-second timeout; interacting with
  the menu can interrupt its countdown and is not proof of a missing timeout.

The shared iPXE build embeds packaged ISRG X1 and USERTrust ECC/RSA roots for
the site's Let's Encrypt/ZeroSSL HTTPS chains. Trust and embed the same root
set; verify the actual served chains in a disposable UEFI iPXE test. TLS
validation remains enabled. Installed copies require a guarded boot-asset
refresh when the shared binary changes; publication alone is insufficient.

The UEFI disk entry must reference the new GRUB installation and remain the
normal firmware boot target. Repartitioning can invalidate the old partition
identity even when its display name still exists; the install role must create
or update the correct entry and verify its loader/partition, rather than relying
on the current Debian entry surviving an Arch installation. Standard UEFI entry
management is distinct from changing vendor BIOS settings. If the required
firmware entry cannot be established, report a blocked final boot and request
manual setup; do not silently declare installation successful.

The netboot entry is not the persistent default. Use grub-reboot homelab-netboot
for the next boot only; do not use grub-set-default to permanently select live.
Read back the requested entry with grub-editenv before issuing a reboot.
Selection is local privileged work by the provisioning role, not a new API
action or permission granted to homelabd.

The supported filesystem/storage layout must allow GRUB to clear its one-shot
environment value at boot. A successful grub-reboot command and readback alone
do not prove that: test consumption across a real reboot. Do not claim support
for arbitrary RAID, encryption or filesystems before validating the environment
block placement. See the [GRUB manual](https://www.gnu.org/software/grub/manual/grub/grub.html)
for next_entry, grub-reboot and environment-block restrictions.

Build one compatible iPXE artifact with an embedded bootstrap script in the
existing boot-infrastructure build/setup path; do not add another API resource
kind or per-Server iPXE build. The embedded script obtains networking, determines
the chosen boot NIC/MAC, and chains the existing /ipxe/<mac> endpoint. It need
not depend on PXE boot filenames or proxy-DHCP because GRUB already loaded iPXE.
The script contains public boot configuration, not a new privileged API token.
See [iPXE embedded scripts](https://ipxe.org/embed).

A locally installed iPXE binary may use its own NIC driver or UEFI network
interfaces; compatibility must be tested on Beelink. Do not assume GRUB launching
iPXE eliminates the NIC priming requirement. Check Secure Boot state and the
actual trust/signing requirements for GRUB -> iPXE; do not silently disable
Secure Boot or TLS validation.

### 5.2 Discovery and API-selected live image

Server.spec.boot.isoRef selects the live execution image.
Server.spec.operatingSystem selects the installed OS. Booting the live image
never independently authorizes installation.

For an unknown/unbound MAC, explicitly configure one existing Ready ISO as a
deployment-level discovery image, initially the existing Arch image. This allows
the agent to report and LLDP binding to resolve MAC -> Machine -> Server. Discovery
is not permission to erase. A manually booted live image can submit the same
reports without first passing through iPXE.

Once bound:

- An active attempt requiring live execution serves its pinned immutable ISO
  build. A newly published ISO or changed CA bundle cannot change that attempt.
- An eligible first-install host without a claimed attempt may receive its
  compatible Ready ISO for enrollment. Claim and pin the actual live build
  before destructive execution.
- A known Server with no eligible/active live request, a paused/disabled request,
  an installed-verification stage, stale binding, or missing required artifacts
  receives a clear non-installing boot error/prompt rather than silently serving
  a new installation environment. Do not fall back to the unknown-MAC image.
- A failed destructive attempt does not automatically become a new eligible
  attempt just because requestedReprovision still exceeds observedReprovision.

The API does not chainload the installed disk. Normal boot and final boot use
GRUB's local installed-OS entry, independent of network/API availability.
Unexpected manual netboot can stop at a recovery prompt; it never records success.

If Beelink's current live session is compatible and its actual build can be
identified and pinned, use it directly. If the session cannot satisfy the
preflight/identity/ISO contract, stop for a deliberate live-image boot; do not
guess its build or reboot hoping a usable GRUB exists. ISO builders/agent reports
must expose a non-secret immutable build identifier if existing reports lack it.

### 5.3 Arming and consuming a reprovision boot

For an installed Server, the operator must:

1. Verify the strict managed SSH identity, installed marker and current boot ID.
   Check disk GRUB, the stable menu ID, iPXE artifact and environment block.
2. Claim/pin the attempt, drain administrative work, and persist PreparingBoot.
   Verify the pinned ISO is reachable and the boot endpoint resolves this exact
   Machine/Server to this attempt before touching local boot selection.
3. Prime and read back the selected NIC, persist live intent/AwaitingLive and
   source boot ID, then arm grub-reboot and verify its readback.
4. Recheck ownership, request, pause state and dependencies before issuing the
   normal OS reboot. If cancellation/preparation fails before reboot, clear and
   verify removal of the pending one-shot entry before releasing the attempt.
5. Await both a changed boot session and the pinned live environment. Port 22
   reopening is insufficient. Use the scoped live SSH handoff in section 8.

Arming and rebooting are not atomic. If the operator crashes between them,
inspect boot ID, pending GRUB entry and attempt checkpoint before deciding what
happened. Do not repeatedly reboot or rearm on each schedule tick. If it is
definitely still the original installed session and no destructive stage began,
safe preparation can resume within the same claimed attempt. An unaccounted
boot/session or uncertain pending entry blocks for inspection.

The one-shot entry is normally consumed before iPXE runs. A networking/API failure
therefore must not leave netboot as the persistent default. Bound boot-script
retries and present a recovery prompt. Another reboot should return to the old OS
if it is intact and one-shot consumption passed hardware acceptance; do not
automatically reboot indefinitely or mark the request successful.

Keep the boot checkpoint within the existing attempt status: source boot ID,
stable menu-entry ID, arming timestamp/readback outcome and expected live build.
It records observed progress, not a command queue or another desired-state
resource. A lost update after arming requires inspection of local grubenv and
the current session; neither API status nor grubenv alone proves a reboot ran.

### 5.4 Live execution, final boot and manual recovery

During installation the live OS remains in memory while the approved disk is
repartitioned. Reinstall GRUB, its environment block, local iPXE artifact and
stable netboot entry as part of the new OS. Verify the persistent default is the
new installed OS and there is no pending next_entry before final reboot.

A whole-disk replacement can temporarily destroy GRUB. That is accepted in v1:
if power is lost or the live session crashes during that window, the owner
manually boots a live image or repairs GRUB. Do not claim that a reboot from an
arbitrary live session will return to live, or that the operator can recover an
unbootable disk through the API. Keep the machine in live until a bootable new
installation is verified; do not schedule casual reboots mid-installation.

An ordinary live reboot is not a persistent live-mode guarantee. Before erasure,
it may boot the previous OS; after erasure and before bootloader installation it
may fail; after successful installation it should boot the new OS. The operator
classifies the actual session and blocks rather than blindly repeating a wipe.

No protected bootstrap partition, permanent USB, firmware PXE-first setup,
iPXE sanboot selection, or PiKVM automation is required. Manual recovery does not
implicitly authorize restarting destructive work: verify the attempt, marker,
disk identity and checkpoint before continuing or requesting a fresh attempt.

### 5.5 Boot acceptance checklist

1. Inspect UEFI/Secure Boot, disk identity, boot NIC/MAC and the current GRUB path.
2. In a disposable UEFI VM, prove normal GRUB boot reaches the installed OS
   with the API/network unavailable.
3. Prove the stable netboot entry chainloads the reviewed iPXE artifact, reaches
   /ipxe/<mac>, and boots the pinned live image without a recursion loop.
4. Prove grub-reboot selects netboot once, GRUB consumes next_entry, and the next
   normal boot defaults to the OS. Test the actual proposed disk layout.
5. Validate Beelink's iPXE NIC support and WoL/EEE priming before an authorized
   reprovision. Check/reconcile competing boot-selection services.
6. Test unknown-MAC discovery, a bound eligible first installation, and refusal
   for stale/unresolved/noneligible known Server requests.
7. Test timeout, cancellation after arming, crash before reboot, accidental
   reboot, and failed installation. No case may silently authorize another wipe.
8. Verify final boot uses the restored disk GRUB and installed marker; document
   the manual recovery procedure for a destroyed bootloader.

These are implementation/acceptance tasks, not changes performed by this RFC.

## 6. Preserve the Beelink NIC workaround

Keep the existing WoL/EEE workaround, but make it explicit and targeted:

- Resolve the boot NIC from its pinned MAC; verify it belongs to this Machine.
  Resolve against current node interfaces on every priming stage, not the cached
  API name: live eth0 can become installed enp1s0. Missing or duplicate MAC
  matches block; recheck the resolved interface MAC before changing settings.
- Before the installed-to-live reboot, enable magic-packet WoL with
  ethtool -s <interface> wol g and, for the affected NIC, disable EEE.
- Read back supported/current settings. Treat required priming failures as a
  blocked boot transition, not blanket ignore_errors.
- Limit the workaround to the boot NIC rather than altering every interface.
- Reapply immediately before relevant reboots; persist an installed-OS unit
  only if hardware testing shows that is necessary.
- Do not use the sysrq hard-reset path as the normal fallback.

The existing comment that WoL forces a NIC to remain in D0 must not become a
design guarantee. The documented operation enables a wake trigger; its effect
on warm-reboot PXE is hardware/driver-specific and must be tested.
[ethtool documentation](https://kernel.googlesource.com/pub/scm/linux/kernel/git/jkirsher/ethtool/+/7afc157973250ae1f53ce634411a170a13e3824b/ethtool.8.in)
describes wol g as magic-packet wake. This is NIC priming, not a requirement to
send a WoL packet for an ordinary OS reboot.

## 7. End-to-end attempt

```text
Pending -> PreparingBoot -> AwaitingLive -> Installing
        -> AwaitingInstalled -> Verifying -> Succeeded
                         failure -> Blocked
```

1. Read the inventory capture group and each captured Server's fresh state.
   Select an eligible initial installation or new explicit reprovision request.
   Resolve dependencies and exact disk. Read-only pending dependencies may be
   retried automatically. Omitted Servers wait; they do not block Beelink.
2. Claim the attempt with UID + If-Match and record counter, inputs, Machine/key
   identities and Concourse build. Reread after claiming before privileged work.
3. Check installed/live state over authenticated SSH, not a discovery report alone.
   Establish the currently trusted boot session and collect disk serial/WWN,
   size, mounts, boot mode and boot NIC.
4. If installed, verify disk GRUB/iPXE, prime the NIC, persist live intent and
   arm/read back the one-shot netboot entry before the normal reboot. If already
   live, verify and pin the actual compatible environment; skip the GRUB/reboot
   step. Neither a live marker alone nor a scheduled tick authorizes erasure.
5. Await a changed boot session and the intended live-image environment over SSH.
   Fresh discovery can refresh the address, not approve a changed machine.
6. Enroll the live instance under the provisioning attempt's scoped SSH transition.
   Verify the managed identity before reading/copying sensitive material or wiping.
7. Persist Installing before destructive work. Revalidate counter, binding,
   dependencies, stable disk identity, mount constraints and pinned plan. Erase
   only the approved physical disk; reject USB/media disks and ambiguous matches.
   Exclude all live-media/USB devices and reject the live runtime's backing disk
   as a target unless the role explicitly proves its execution is independent
   of that disk. The existing SSD's partitions require intentional replacement.
8. Partition/install using the existing roles. Preserve the managed host key,
   CA bundle, fixed ansible account service/timer and restricted daemon token.
   Write a root-owned installation marker containing Server UID, attempt ID,
   request counter, root filesystem identity and applied non-secret plan digest.
9. Install/verify disk GRUB, installed-OS default, stable netboot entry, local
   iPXE artifact and writable/consumable environment block. Clear pending
   next_entry. Record the root/EFI partition identities and loader path.
   Persist AwaitingInstalled/installed intent and the live boot ID before final
   reboot. Firmware -> disk GRUB -> installed OS is the intended final path.
10. Verify a fresh strict managed-key connection, a changed boot ID, installed
    root (not overlay/live), installation marker, expected OS and critical
    management services. Finish required post-provision work without another wipe.
11. Conditional /status update: mark Succeeded/provisioned, record the successful
    snapshot and set observedReprovision to this attempt's counter (0 for initial
    installation). Report through the generic /status endpoint.

observedServerGeneration describes the successful execution snapshot, not an
assertion that unrelated concurrent edits were installed. Do not substitute the
latest counter or current generation when completing an older attempt.

## 8. SSH handoff and interruption safety

An installed OS and a new live image can initially serve different SSH host keys.
Do not globally disable checking or automatically clear managed trust on failure.

Use an explicit transition bound to the active attempt, Server/Machine UIDs,
expected address/boot NIC, pinned ISO build and boot session. First contact with
the fresh live image remains scoped TOFU in v1, with its documented impersonation
risk. Persist its pin before managed private-key delivery, install the stable
managed key, then strictly verify it. Never repurpose an arbitrary key mismatch
as permission to reenroll or provision.

The SSH operator skips a Server while a provisioning attempt owns its SSH/boot
transition. Put the SSH and provisioning jobs in the same existing pipeline,
both with serial_groups: [server-lifecycle]. This serializes their actual builds,
including manual triggers, and avoids a new lock resource or a second pipeline.
Concourse documents this same-pipeline behavior in its
[job serial_groups reference](https://concourse-ci.org/docs/jobs/).
Independent serial jobs would not provide cross-job exclusion; a status flag
alone cannot exclude an already-running SSH pass. Normal Commands refuse targets
in a transition rather than initiating their own TOFU enrollment.

A queued/in-flight administrative Command can also race a reprovision. Before
starting a reboot/erase, require that managed command builds touching the host
are drained; reserve the target so new normal runs refuse it. Inventory groups
overlap, so checking only one CommandsPipeline is insufficient. In v1 use one
documented maintenance gate and supported execution path, not claims of a
distributed lock that is not implemented.

Recovery rules:

- Before destructive stages: re-inspect and retry only safe preparation; account
  for an armed GRUB next_entry and source boot ID. Before releasing an attempt,
  clear any pending netboot selection on a reachable installed host. If cleanup
  cannot be verified, report unresolved boot selection and block new attempts.
- AwaitingInstalled/Verifying: inspect the marker and strict managed identity;
  resume verification without repeating installation.
- Interrupted Installing: block automatic retry unless inspection proves an
  explicit safe resumable checkpoint. Missing status or failed Ansible is not
  proof that no erase occurred. In v1 stop for operator intervention.
- Lost completion PATCH: verify the installed marker and finish status; do not
  erase again because requested still exceeds observed.
- Binding/key replacement, unknown boot key, expired/mismatched attempt,
  changed disk identity or uncertain ownership: block.
- No exactly-once erase guarantee is possible across host/API crashes merely by
  adding counters or checkpoints. Favor refusing an uncertain retry over a
  second destructive run.

An API/Concourse snapshot restore must not silently replay destructive requests.
Disable the provisioning operator during restores; reconcile installed markers
against request/attempt state before reenabling execution.

## 9. Implementation and acceptance sequence

1. Add schemas, API transition validation and tests; regenerate docs/clients.
2. Refactor the playbook into bounded, observable stages; remove ignored boot
   failures and implicit API/Server creation from the execution-only path.
3. Implement discovery and attempt-pinned /ipxe routing, including unknown
   MAC -> report -> LLDP binding and refusal for noneligible known requests.
4. Standardize the disk GRUB/iPXE role and test one-shot environment consumption,
   OS-default final boot, API-independent normal boots, and targeted NIC priming.
   Do not require firmware PXE or permanent USB for the current live-first path.
5. Implement scoped SSH transition, shared execution coordination and markers.
6. Add the provisioning job to the shared lifecycle pipeline; start with no request.
7. Run preflight-only tests: partial inventory, disk ambiguity, stale UID/ETag,
   missing ISO/CA, operator overlap, failed NIC priming and unreachable host.
8. In a disposable VM, prove an eligible fresh host installs once at counter 0,
   unknown existing disks block automatic erasure, request 1 reinstalls once,
   ordinary edits do not reinstall, and crashes/retries cannot blindly rewipe.
9. With a separately approved Beelink disk identity and explicit counter bump,
   verify live -> installed -> live -> installed, strict SSH continuity,
   daemon enrollment, restored OS-default GRUB and observedReprovision advancement.

Open implementation choices: exact bounded snapshot schema, supported GRUB
environment-block layout, reviewed UEFI iPXE build/distribution path, immutable
live-build reporting, and stable disk/layout discovery. Resolve these before
implementing destructive execution. Manual recovery is accepted; automatic
bootloader/disk-failure recovery is outside v1.

## 10. Read-only Beelink observations, 2026-10-06

Collected over SSH as the fixed ansible management user, with strict API-derived
host-key trust. The account is available for further bounded read-only inspection;
provisioning/data-loss approval remains separate. No reboot, NIC-setting change,
boot-order write, disk mount or erase was performed.

- Bound management address: 10.1.1.243; x86_64, UEFI.
- DMI identifies AZW/Beelink EQ13, with American Megatrends firmware EQ13D403,
  dated 2024-05-13.
- efivarfs is mounted read-write. Standard UEFI BootOrder/BootNext management
  through efibootmgr is a candidate, but actual firmware acceptance and persistence
  have not been tested; no variable writes were performed.
- No /sys/class/firmware-attributes interface is exposed by the current live
  kernel. WMI devices exist, but that alone does not establish a supported BIOS
  settings API. Do not assume Linux can enable the firmware network stack or
  change arbitrary Setup settings. Those require a verified vendor interface or
  one-time manual firmware configuration; raw vendor-variable edits are excluded.
- Current root is the Arch live overlay; /var/lib/is_live_env exists.
- BootCurrent 0004 is USB optical media, not a PXE boot.
- BootOrder is 0003,0002,0004,0001: Debian disk, USB flash, USB optical, EFI shell.
  No PXE/network boot entry was shown by efibootmgr -v. This does not prove
  firmware PXE support is absent. Firmware PXE is optional recovery/bootstrap,
  not a prerequisite for GRUB-based reprovisioning.
- Management/boot NIC MAC e8:ff:1e:d4:03:fa resolves to enp1s0; r8169 driver,
  firmware rtl8168h-2_0.0.2; 1 Gb/s link.
- WoL supports magic packets but currently reads Wake-on: d (disabled).
- EEE is currently enabled and active. The proposed priming must target this NIC.
- SATA disk is a 476.9 GiB-class 512GB SSD, serial MP23B72602251. Stable aliases:
  /dev/disk/by-id/ata-512GB_SSD_MP23B72602251 and
  /dev/disk/by-id/wwn-0x53a5a277260208cc, both currently resolving to /dev/sda.
- That disk already has EFI, swap and ext4 partitions. This is not a fresh blank
  target; do not automatically erase it under the initial-install path.
- A separate 14.9 GiB USB flash device and optical boot media must be excluded
  from target-disk selection.

These are point-in-time observations, not data-loss approval or guaranteed future
device names. Revalidate immediately before an eventual authorized execution.
PiKVM is not part of the operator/design; SSH alone was used for this inspection.

## Implementation progress (2026-10-06)

The API now implements the conditional monotonic request counter, guarded attempt
checkpoints and UID-pinned live boot routing. The existing ssh-managed operator
pipeline has a provisioning job, sharing its lifecycle serial group. Operator
code lives in ansible-roles/operators/provisioning.py; stage recipes and probes
live in the provision, management and grub roles. Initialization builds the
embedded-script UEFI iPXE artifact; live ISO builders record immutable build IDs.
The rollout checklist is stigmergy/docs/server-provisioning-rollout.md.

V1 conservatively stops all API Command dispatch while any Server is reserved,
drains submitted Commands, and refuses reserved targets in the normal runner.
This is not a universal lock on administrator SSH or directly triggered backend
jobs. Installation is limited to amd64 UEFI (verified Secure Boot off), supported
Arch/Debian releases and an explicit EFI/swap/ext4 SATA/NVMe layout. Unknown
firmware state, missing protected daemon enrollment, unsupported inputs and
unverified live build/session block erasure.

API/operator regression tests pass. A real network-isolated UEFI VM fixture
verified that GRUB consumes the one-shot netboot selection and then returns to
the local installed-OS menu default. This is not a full OS-install or hardware
reprovisioning acceptance test. The pinned iPXE artifact compiles successfully.
Full disposable-VM installation/reinstallation and physical NIC/netboot/SSH
handoff acceptance remain required before production activation.

A new read-only Beelink probe confirms its SSD remains partitioned and its older
live image has no immutable live-build marker. A rebuilt live image is required,
and replacing that existing disk needs explicit authorization. Provisioning
remains disabled; no reboot or erasure was performed as part of implementation.

## Beelink rollout checkpoint (2026-10-07)

The owner explicitly approved replacement of Beelink only, using the managed-node
playbook and the stable SSD identity recorded above. Both refreshed ISO builds
completed and the API/operator resources were deployed with existing API tokens
and Git SSH identity preserved. The site request counter is now 3: two preparation
attempts stopped before reboot/erasure, exposing missing live package indexes and
PTY contamination of JSON probe output. Those issues are fixed and tested.

The third attempt loaded the pinned Arch kernel/initramfs through Ansible kexec
stages and requested the live transition, but Beelink did not reconnect. The
operator timed out before Installing and retained Blocked/maintenance/netboot
intent. No partitioning or erasure stage was entered. Recovery now requires a
manual restart/console or booting the refreshed USB/live ISO; PiKVM is excluded.
Do not clear maintenance or increment the counter without inspecting recovery.

A diskless VM reproduced the early DHCP failure. The netboot recipe was missing
Arch's upstream boot-interface selection: net.ifnames=0 plus BOOTIF selecting the
verified NIC. Both the API's iPXE rendering and the Ansible kexec command now
include those options. The corrected VM obtains DHCP and downloads the HTTPS
root filesystem. A 2 GiB fixture exhausted RAM after the 1 GiB rootfs download;
a 4 GiB fixture subsequently reached the live login prompt and started OpenSSH.
This verifies the corrected network-live boot path, not physical Beelink recovery
or successful installed-system provisioning.

## Completed Beelink installation (2026-10-07)

After owner-reported manual USB recovery, the original SSD identity and changed
live boot were verified and the existing managed SSH key restored without key
rotation. Request 4 successfully reached the pinned new Arch live build. Its
SSH handoff exposed a reload/reconnection trust gap; enrollment now accepts both
recorded bootstrap and verified managed keys only during that transition.

Request 5 installed the Arch base on the approved SSD, then stopped at an empty
sysctl copy template. A guarded configuration-only repair verified the original
live session/disk and existing root/EFI/bind mounts, completed configuration,
management, GRUB and the staged marker without a second wipe. The same owned
attempt resumed installed-boot verification with its original pinned code.

The final Concourse provision build 17897 succeeded. An independent strict SSH
probe confirmed a new installed boot, ext4 root on SSD MP23B72602251, matching
request-5 marker and restored GRUB. API status is Succeeded with
observedReprovision=5 and maintenance/netboot intent cleared. The USB and other
physical nodes were not provisioned. The temporary Concourse code pin was
removed and latest code checked; future runs stream standard no_log-aware
Ansible task/results/recap output and immediate stage/checkpoint progress.
Another replacement requires a new explicit request; this success does not
authorize one. Full repeated-reprovision and separate Debian installation
acceptance tests remain distinct from this completed first physical install.

## Beelink boot-only recovery after request 8 (2026-10-07)

Request 8 armed GRUB and rebooted, but timed out before the verified live ISO
returned; no installation stage ran. iPXE trusted only ISRG X1 while the HTTPS
proxy served a ZeroSSL chain rooted in USERTrust ECC. The shared binary now
embeds/trusts packaged ISRG X1 and USERTrust ECC/RSA. A disposable UEFI iPXE
fixture successfully fetched both the API and artifact-server HTTPS responses.

After manual installed-OS recovery, strict SSH verified the unchanged request-5
marker, root UUID and SSD serial/WWN/size. A guarded Ansible boot-only repair
refreshed `/boot/ipxe/ipxe.efi`, regenerated/validated the five-second menu and
cleared next_entry, without rebooting or installing. The five-second timeout
was already present; the reported missing countdown was not reproduced.
Maintenance/netboot intent were then cleared through conditional status PATCH.
Request 8 remains Blocked, observedReprovision remains 5, and a new explicit
request is required. Physical end-to-end GRUB/iPXE reinstallation is not yet
validated by this TLS VM test or boot-only repair.
