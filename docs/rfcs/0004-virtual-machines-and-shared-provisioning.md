# RFC 0004: Virtual machines with a shared Server provisioning lifecycle

- Status: Draft; recorded for future implementation
- Created: 2026-10-07
- Related: RFC 0002 (external operators), RFC 0003 (provisioning runs)
- Scope: explicit VM resources, optional ConnectX-5 SR-IOV networking, and
  reuse of the physical-machine management/provisioning flow

## Decision

Virtual machines need explicit resources describing their virtual hardware.
They should otherwise behave like physical managed machines: discovery,
Server binding, SSH enrollment, inventory capture, disk selection,
ProvisioningRun checkpoints, OS installation and reprovisioning use the same
contracts and reviewed playbooks.

Separate creating a machine from installing its OS. A VM lifecycle operator
creates virtual hardware; the existing provisioning operator installs the OS.
Do not introduce VMProvisioningRun, a second OS installer, per-VM pipelines,
or a generic workflow engine.

This document records the agreed direction, not implemented API fields or
authorization to create VMs, change host networking, reboot or erase anything.

## Resource boundaries

| Resource | Responsibility |
| --- | --- |
| Hypervisor | Execution target and backend connection/configuration; initially one KVM/libvirt host |
| VirtualMachine | CPU, RAM, firmware, virtual disks, network attachments and desired power state |
| Server | Desired managed OS, boot ISO, SSH CA, users/settings and provisioning gate |
| Machine | Actual discovered guest hardware and addresses reported by homelabd |
| ProvisioningRun | Explicit immutable authorization to install onto selected discovered disks |
| Existing Pipeline/PipelineProvider and InventoryCaptureGroup | Shared external execution and target selection |

Hypervisor and VirtualMachine are proposed new resource kinds. Other kinds
already exist. A Hypervisor may reference a managed host Server and existing
Secret/SecretStore credentials rather than duplicate host inventory or embed
credentials. Final backend connection fields remain a schema-design task.

A Server references its VirtualMachine through a proposed UID-qualified
virtualMachineRef. Physical Servers continue using their machine selectors.
These binding modes must be mutually exclusive. The VM does not duplicate the
Server's OS, CA or installation configuration. Its hypervisor-side power state
does not assert that the guest OS or management SSH is healthy.

Keep the VM independently manageable. Do not automatically delete a VM or its
disks when its Server is deleted; destructive lifecycle policy must be explicit.

## Minimal virtual hardware contract

Start with one hypervisor, amd64 UEFI, one system disk and one boot-capable NIC.

VirtualMachine desired inputs:
- Hypervisor reference.
- CPU count, memory and firmware mode.
- Virtual disks with stable per-disk serials and explicit capacities.
- Network attachments with stable MACs: bridged or SR-IOV.
- Desired running/stopped state.

VirtualMachine observed status:
- Observed generation, conditions and useful failure messages.
- Stable domain UUID tied to the VirtualMachine resource UID.
- Realized disk serials and interface MACs.
- Assigned VF identity where applicable, including host PCI address.
- Actual power state and virtual-hardware readiness.

These are conceptual fields, not a finalized YAML/API schema. Keep storage
backend details behind the hypervisor implementation. Disk growth, shrink,
replacement and deletion must not be silently treated as harmless reconciliation.
Existing UID-qualified ProvisioningRun approval never authorizes changing the
VM's disk backing to another device.

## Binding and discovery

A VM sharing a physical uplink cannot be distinguished by LLDP switch/port
alone. Extend binding to match the expected domain UUID and management/boot MAC
published by the VM operator against guest discovery. Require a unique match
and preserve reciprocal Server/Machine references.

Use a stable administrator-configured MAC for preboot DHCP/iPXE selection.
Use the hypervisor domain UUID as an additional runtime identity observation.
Verify what the guest reports in the actual UEFI/live fixtures before finalizing
the selector. Reject ambiguous matches; do not fall back to a shared uplink.

Identity remains lifetime-bound: recreating a resource with the same name must
not inherit a prior UID's domain, reservation or SSH identity automatically.
MAC/UUID discovery is identification, not cryptographic attestation. The
existing scoped TOFU enrollment limitations still apply.

Management-interface selection must work by the expected interface identity,
not require a physical LLDP attachment. InventoryCaptureGroup stays generic;
no VM-specific provisioning or SSH fields belong in that resource.

## Networking and ConnectX-5 SR-IOV

Bridged virtual NICs are the first boot/provisioning fixture. They give guests
independent MACs and LAN addresses and can participate in DHCP/iPXE without
SR-IOV. This is also the fallback if guest VF preboot support is unavailable.

SR-IOV is an optional hardware-backed attachment. A VM references a named,
hypervisor-local VF pool; it does not normally request a hardcoded PCI address.
Initially configure pool membership on the hypervisor. Do not add a standalone
VFPool API resource until an actual allocation-management need justifies it.

The VM operator assigns a free VF, records the allocation against the VM UID,
sets/preserves its MAC, and reuses that allocation across restarts. Allocation
must be exclusive and recoverable after an operator crash or host reboot:
inspect existing domain attachments and ownership before assigning a device.
Pool exhaustion reports a condition rather than stealing another VM's VF.

Host prerequisites and acceptance checks:
- CPU/chipset IOMMU support and host/card SR-IOV firmware configuration.
- Safe VFIO binding and suitable IOMMU isolation.
- Correct provisioning LAN/VLAN connectivity.
- Stable VF MAC configuration after a host reboot.
- Confirmed guest preboot support for the exact VF/device/firmware combination.

ConnectX-5 SR-IOV support does not by itself prove that a guest can use the
physical card's PXE option ROM. Do not promise direct VF iPXE boot before testing.
If required, use a bridged boot/management NIC plus a VF for runtime networking;
make NIC roles explicit so provisioning selects the correct boot MAC.

VF reassignment is a disruptive operation. Do not change host PF configuration
or remove its networking automatically as part of ordinary VM reconciliation.
No live-migration support is promised for assigned hardware in v1.

## Shared provisioning flow

1. Create Hypervisor and VirtualMachine resources, then a linked Server with
   desired OS, boot ISO, CA and provisioning configuration.
2. The VM operator creates hardware and starts the guest. First boot enters the
   configured discovery/live environment through the existing iPXE path.
3. homelabd reports the guest as a Machine; binding resolves it to its Server.
4. The existing SSH operator installs and verifies the Server-owned identity.
5. The UI displays discovered guest disks. The owner selects the system disk
   and confirms a new ProvisioningRun, exactly as for a physical Server.
6. The existing provisioning operator verifies the live session and disk identity,
   installs the OS and the normal GRUB/iPXE contract, and verifies installed boot.
7. Later runs use the same installed -> one-shot GRUB/iPXE -> live -> installed
   flow. Normal boot remains local, not dependent on the API or network.

Creating/starting a VM or enabling Server provisioning does not authorize OS
installation or disk replacement. No default cloud image or cloud-init OS
installer bypasses the shared flow. Manual boot/live-media recovery remains an
accepted v1 fallback; no PiKVM integration.

Current disk checks intentionally allow physical SATA/ATA/NVMe targets only.
Virtual disks require an explicit, narrowly validated extension, not disabling
those checks. Match the selected guest serial, size and stable by-id alias to
the VM operator's realized disk identity. Recheck mount state, read-only status,
boot identity and run ownership immediately before mutation. A host-side
verification must also reject raw host-device backing in this initial design.

Provisioning affects only the authorized guest disk. Physical disk safeguards
remain intact, and VM replacement never implicitly authorizes physical-host
erasure. Guest discovery alone does not prove safe backing or execution target.

## Controllers, operators and code locations

- stigmergy: new source resource schemas/generated CRUD/status/OpenAPI; resource
  relationship validation; Server/Machine binding and management-interface
  selection extensions; VM-aware boot lookup and provisioning disk validation.
- ansible-roles: a bounded VM lifecycle operator with reviewed libvirt/host roles.
  Keep orchestration in operators and host/guest mutations in Ansible.
  Extend existing provisioning probes/roles only for the supported virtual-disk
  identity checks; reuse OS installation, SSH, GRUB/iPXE and checkpoint stages.
- homelabd: report the required domain/guest and virtual-disk identities, if
  existing inventory is insufficient. No VM creation or provisioning execution.
- homelab-init: Hypervisor/VM/Server manifests, generic capture groups and one
  persistent shared VM reconciliation pipeline.
- stigmergy-web: VM status/linkage and supported disk presentation, while reusing
  the Server Provision dialog and ProvisioningRun checkpoint display.

Use the existing external operator pattern: scheduled/manual Concourse builds,
one bounded pass, live progress, generic UID-bound /status updates and conditional
writes. No separate pipeline per VM. Hypervisor readiness or a green build does
not mean guest provisioning succeeded.

Coordinate VM power/device changes with active ProvisioningRun ownership.
Do not stop/recreate a guest, replace its disks or reassign its NIC while a run
owns it. Guest reboot during provisioning stays in the reviewed guest playbook;
ordinary VM reconciliation must not fight that reboot.

## Implementation order and acceptance

1. Prove one manually created bridged UEFI VM can boot the existing iPXE/live ISO.
2. Implement minimal Hypervisor/VirtualMachine resources and the external operator.
3. Add stable binding, management-interface selection and virtual-disk safeguards.
4. Complete one end-to-end VM install and reprovision through existing resources.
5. Add VF allocation, test guest preboot support, then test the same flow with
   SR-IOV or the explicitly configured bridged-boot fallback.

Required tests include ambiguous identity, name/UID replacement, duplicate MAC,
disk mismatch, unsafe backing, VF double allocation/exhaustion, host restart,
maintenance conflicts, interrupted installation and cleanup. Verify guest disk,
installed marker, SSH identity and run status, not merely a successful trigger.

Initial acceptance uses disposable VM-backed storage only. It does not authorize
a new Beelink provision or modification of the host's physical disks.

## Deferred decisions

- Exact Hypervisor connection schema and local VF pool configuration.
- Tested guest UUID source, preboot MAC binding and identity freshness rules.
- Safe backing verification and disk-serial visibility for supported libvirt disks.
- Direct ConnectX-5 VF UEFI/iPXE support on the actual host and card.
- Explicit disk/VM deletion confirmation and retention behavior.
- Multi-host scheduling, migration, snapshots, clones and richer storage layouts.

Keep v1 small. Add these only when needed; preserve one Server lifecycle and one
ProvisioningRun model throughout.
