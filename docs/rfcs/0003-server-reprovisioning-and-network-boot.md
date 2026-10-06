# RFC 0003: Server reprovisioning and network-boot orchestration

- Status: Draft; no provisioning implementation or hardware changes authorized by this RFC
- Created: 2026-10-06
- Scope: initial provisioning, explicit reprovision requests, API state, external execution,
  PXE boot selection, and the installed/live SSH transition
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

Every normal reboot enters a persistent iPXE bootstrap first, whether the
previous environment was a live ISO or an installed OS. Configure firmware once
to prefer network boot or a dedicated persistent iPXE USB. Both lead to the same
API boot decision. Initial discovery, live provisioning and installed-OS boot
are different outcomes of that decision, not different provisioning systems.

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
- /ipxe/<mac> currently serves a configured live ISO regardless of whether a
  reprovision is active: this would loop with permanent firmware PXE-first.
- Existing GRUB snippets disagree on names/IDs and chainloading. Passing iPXE
  commands as GRUB chainloader arguments is not a validated substitute for
  an embedded iPXE script or DHCP-delivered script.
- Existing Linux-priority boot services can set BootNext to the current disk
  entry and conflict with a deliberate network-boot request.
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
    bootTarget: live
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
- stigmergy/internal/api/ipxe.go: lifecycle-aware live/local boot routing.
- Existing Server controllers: dependency readiness and identity relationships,
  preserving operator-owned provisioning status.
- ansible-roles/operators/provisioning.py: bounded orchestration, attempt claiming,
  stage validation, SSH transitions, recovery and generic /status reporting.
- ansible-roles/plays/provision.yml and roles/provision: reusable installation
  stages, not a second provisioning implementation in Python.
- ansible-roles/roles/grub: one validated stable iPXE entry and GRUB environment.
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

## 5. Boot design: always enter the persistent iPXE bootstrap

Choose either persistent entry path during the machine's one-time setup:

```text
Firmware default network boot -> iPXE --+
                                       +-> API boot decision -> live ISO
Firmware default boot USB ----> iPXE --+                     -> installed OS
```

For network boot, firmware fetches the iPXE EFI binary from boot infrastructure.
For USB boot, a small persistent boot image contains iPXE and its bootstrap
script. The USB is not the selected distro's live image; it only obtains an
address and contacts the common API-backed boot endpoint. An embedded script
can contact that endpoint without relying on a DHCP-provided boot filename.
See [iPXE embedded scripts](https://ipxe.org/embed).

Configure firmware once to prefer the chosen entry path, with an explicit tested
recovery/fallback policy. An iPXE image cannot override firmware selection.
UEFI variable writes from Linux may work even without a programmable firmware
settings UI; test them rather than assuming they work. If unavailable, select
network/USB first manually once. OS reboots and live-image reboots then use the
same persistent entry without repeated BIOS interaction. A one-boot BootNext
override alone does not establish this invariant.
[efibootmgr documentation](https://github.com/rhboot/efibootmgr/blob/main/README)
distinguishes persistent BootOrder from BootNext.

Server.spec.boot.isoRef selects the live execution image;
Server.spec.operatingSystem selects the OS installed on the target disk. They
must not be conflated. The API decides when to serve live artifacts versus the
verified installed loader. A long-running live instance therefore returns to
live after an ordinary reboot for as long as that is the API's boot decision.
Booting live never independently authorizes erasure.

The preferred bootstrap lives outside the replaceable OS. A dedicated USB must
remain attached and must be excluded from every wipe/partition operation. With
firmware network boot, the bootstrap is on the network, not on the OS disk.
Disk GRUB remains the installed OS loader, not the control point that must be
recreated before a live instance can reboot. Keep/reconcile existing GRUB iPXE
entries only as deliberate recovery tools; grub-reboot is not required on every
reprovision in this model.

If a disk-resident iPXE bootstrap is added later, its EFI partition must be
protected independently of root installation and whole-disk erasure must be
replaced with scoped partition operations. That is not a requirement for the
network/USB v1 entry paths.

Lifecycle-aware /ipxe/<mac> behavior:

There is a first-boot discovery dependency: MAC -> Machine -> Server requires
the live agent to report before the LLDP-based Server binding can exist. Firmware
PXE cannot submit that report. For an unknown/unbound MAC, explicitly configure
one existing Ready ISO as a deployment-level discovery image (initially the
existing Arch image). Boot it only for discovery/enrollment, never installation
without a bound, eligible Server. It is not a new resource kind or a per-host
pipeline. Do not use this discovery fallback for a known Server with unresolved
or replaced ISO/CA references. Once bound, use its selected compatible boot ISO
and install its spec.operatingSystem. The image supplies a compatible trusted
management CA; validate that enrollment can proceed before selecting it.

Routing after binding:

- Active, dependency-ready attempt in a live-required stage: serve the pinned
  immutable ISO build from the attempt, not whichever build is newest now.
- Eligible first installation before claim: serve the Server's compatible Ready
  boot ISO for discovery/enrollment; claim and pin its actual build before any
  destructive operation. Counter 0 must not route such a host straight to disk.
- Already provisioned with no newer request, provisioning disabled, or final
  installed-boot stage: return to the local disk path. An unsafe failed attempt
  is not silently reauthorized by this routing decision.
- Invalid/stale binding, missing pinned artifacts, or unresolved required state:
  report the error and perform no destructive operation. Operator preflight
  must refuse the reboot; unexpected network boot needs a tested recovery path.

For installed boot, select the installed OS's distinct EFI loader and partition
identity directly from iPXE. Do not reboot or restart the USB/firmware iPXE entry;
that would loop back to the API. iPXE's sanboot supports UEFI filename/partition
selection and local boot, but the exact loader/partition selection must be
tested on Beelink. Do not assume BIOS drive 0x80 identifies the SSD when a USB is
also present. See [iPXE sanboot](https://ipxe.org/cmd/sanboot).

Bound network/API failures and use only a previously validated local loader as
fallback, or present a recovery prompt when no installed loader is available.
Fallback never authorizes installation and never records provisioning success.
Do not depend on exiting iPXE to make firmware select the correct next device.
TLS capabilities, Secure Boot compatibility and this NIC's driver behavior
remain hardware acceptance requirements, not guarantees of an image format.

Beelink PXE acceptance checklist:

1. Inspect efibootmgr -v, actual boot NIC/MAC, firmware mode and Secure Boot.
2. Validate proxy-DHCP on Beelink's actual L2 segment, avoiding a competing DHCP
   lease server. Verify UEFI client architecture matching and next-server access.
3. Firmware PXE clients receive a bootable iPXE EFI binary; iPXE clients receive
   boot.ipxe. Distinguish them to prevent chainloading iPXE into itself.
4. Check HTTP/TFTP/TLS capabilities of the selected iPXE binary and chain path.
   Do not silently disable TLS verification to make boot work.
5. Verify MAC -> Machine -> Server UID resolution and compatible Ready ISO/CA.
6. Test persistent entry -> live, live reboot -> live, installed reboot -> iPXE
   -> installed, and network/API failure without a bootstrap recursion loop.
7. Disable/reconcile competing Linux-priority BootNext writers.
8. Test live-to-live and the installed loader handoff non-destructively; verify
   a full installed-to-live-to-installed cycle on a disposable VM before an erase.

Do not change boot order now simply because a reprovision was requested in prose.
These are implementation/acceptance tasks, not changes performed by this RFC.

## 6. Preserve the Beelink NIC workaround

Keep the existing WoL/EEE workaround, but make it explicit and targeted:

- Resolve the boot NIC from its pinned MAC; verify it belongs to this Machine.
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
4. Verify the persistent network/USB bootstrap and prime the boot NIC. Commit
   the live boot target and stage before issuing the normal reboot. If already live, verify
   that it is the intended live environment before proceeding.
5. Await a changed boot session and the intended live-image environment over SSH.
   Fresh discovery can refresh the address, not approve a changed machine.
6. Enroll the live instance under the provisioning attempt's scoped SSH transition.
   Verify the managed identity before reading/copying sensitive material or wiping.
7. Persist Installing before destructive work. Revalidate counter, binding,
   dependencies, stable disk identity, mount constraints and pinned plan. Erase
   only the approved physical disk; reject USB/media disks and ambiguous matches.
   Record the selected persistent bootstrap and explicitly exclude its device.
8. Partition/install using the existing roles. Preserve the managed host key,
   CA bundle, fixed ansible account service/timer and restricted daemon token.
   Write a root-owned installation marker containing Server UID, attempt ID,
   request counter, root filesystem identity and applied non-secret plan digest.
9. Install the OS's GRUB/EFI loader without displacing the persistent iPXE entry.
   Record its verified partition identity and loader path. Commit installed
   boot target before final reboot; iPXE selects that distinct loader rather
   than booting live again or recursively booting the bootstrap.
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

- Before destructive stages: re-inspect and retry only safe preparation.
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
3. Implement discovery-image bootstrap, lifecycle-aware pinned PXE routing and
   local fallback tests, including unknown MAC -> report -> LLDP binding.
4. Test Beelink's persistent network/USB entry, live-to-live reboot, installed
   loader selection, boot order and targeted WoL/EEE priming non-destructively.
5. Implement scoped SSH transition, shared execution coordination and markers.
6. Add the provisioning job to the shared lifecycle pipeline; start with no request.
7. Run preflight-only tests: partial inventory, disk ambiguity, stale UID/ETag,
   missing ISO/CA, operator overlap, failed NIC priming and unreachable host.
8. In a disposable VM, prove an eligible fresh host installs once at counter 0,
   unknown existing disks block automatic erasure, request 1 reinstalls once,
   ordinary edits do not reinstall, and crashes/retries cannot blindly rewipe.
9. With a separately approved Beelink disk identity and explicit counter bump,
   verify live -> installed -> live -> installed, strict SSH continuity,
   daemon enrollment, local boot fallback and observedReprovision advancement.

Open implementation choices: exact bounded snapshot schema,
hardware-supported boot strategy/fallback, and stable disk
ID/layout discovery. Resolve these before implementing destructive execution.

## 10. Read-only Beelink observations, 2026-10-06

Collected over SSH as the fixed ansible management user, with strict API-derived
host-key trust. The account is available for further bounded read-only inspection;
provisioning/data-loss approval remains separate. No reboot, NIC-setting change,
boot-order write, disk mount or erase was performed.

- Bound management address: 10.1.1.243; x86_64, UEFI.
- Current root is the Arch live overlay; /var/lib/is_live_env exists.
- BootCurrent 0004 is USB optical media, not a PXE boot.
- BootOrder is 0003,0002,0004,0001: Debian disk, USB flash, USB optical, EFI shell.
  No PXE/network boot entry was shown by efibootmgr -v. This does not prove
  firmware PXE support is absent; network-stack settings/entry creation need work.
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
