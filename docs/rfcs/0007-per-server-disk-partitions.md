# RFC 0007: Per-server disk partitions

## Boundary

DiskPartition describes desired layout. ProvisioningRun alone authorizes whole-
disk replacement. Creating, editing or deleting a partition resource never
formats, resizes or removes physical storage. No background storage installer,
partition-specific pipeline or additional Disk resource is needed.

## Resources

```yaml
apiVersion: homelab.io/v1alpha1
kind: DiskPartition
metadata: {name: node-root}
spec:
  serverRef: {name: node}
  diskSelector: {serial: discovered-serial}
  number: 3
  capacity: {storage: 10Gi}
  role: root
  filesystem: ext4
  mountPoint: /
```

Names are globally unique. serverRef may include UID to reject replacement;
the run always pins Server and Machine UIDs. Select by serial, WWN or both
(conjunctive matching); paths are observations, never disk authorization.
Selectors must match exactly one disk in the bound Machine inventory and that
disk must equal the disk explicitly selected in the run.

Roles are root, efi, swap and data. Root uses ext4 at /; EFI uses vfat at
/boot/efi; swap has no mount point; data uses ext4 at a clean absolute mount
point outside reserved virtual/boot mounts. Numbers are explicit and unique,
1–128, and determine physical order. At most 64 resources are reviewed per run.
Exactly one root and one EFI are required for the supported UEFI installer.
Swap is optional. Mount points cannot repeat.

Capacity is either a positive whole Mi/Gi/Ti quantity or useRemaining: true.
Only the last partition can consume remaining capacity. Geometry is aligned
to whole MiB, with the first and final MiB reserved for alignment/GPT metadata.
EFI requires at least 260Mi; root requires at least 1Gi. Fixed capacity may leave
unallocated space. Filesystem free space is not equal to partition capacity.

## Confirmation and execution

1. Discovery reports disks and partition/filesystem observations into Machine
   status; the Server remains bound to that Machine.
2. The UI selects a discovered disk and displays this Server's matching layout.
   Generic resource forms configure DiskPartitions. Opening the dialog writes
   nothing; incomplete layouts cannot submit.
3. ProvisioningRun.spec.storage contains one approved disk and partitionRefs
   with names, UIDs and reviewed generations. The API reads exactly this explicit
   set; additions after confirmation are not implicitly included.
4. Validation checks identities, selectors, roles, mounts, ordering and fit.
   A single etcd transaction compares Server, Machine and partition read versions,
   creates the run with immutable status.storagePlan, and reserves the Server.
   Any concurrent change conflicts without leaving a run or reservation.
5. The operator consumes this frozen plan, not capture-group storage variables.
   Its immutable attempt snapshot pins normalized execution inputs and reviewed
   operator revision. Later resource edits/deletions describe future state and
   cannot alter the accepted run.
6. RFC 0003's fresh pinned live boot and changed boot ID are mandatory before
   erasure. Serial/WWN/size, by-id path, mounts and active swap are rechecked.
7. Reviewed Ansible roles create exact numbered partitions, format filesystems,
   mount by declared location and generate UUID-based fstab. EFI registration
   and installation verification use declared roles, not fixed partition indices.
8. Installation and final installed verification check the complete partition
   set, geometry, filesystem identities and declared filesystem mounts, as well
   as the existing OS/GRUB/management/agent contract.

The whole approved disk is replaced, including existing partitions absent from
the new layout. Deleting a DiskPartition resource has no physical effect.
Changing storage desired state never creates a run or automatically retries erasure.

## Scope and rollout

Support covers one writable nonremovable SATA/ATA/NVMe/MMC user-area disk, amd64 UEFI with Secure Boot
off, Arch rolling or Debian trixie. Multiple Servers may have independent layouts.
Raspberry Pi/ARM, multiple disks, RAID, encryption, other filesystems and runtime
resize are not implemented by this storage model alone.

Discovery reports all detected devices unchanged. API and UI exclude MMC
hardware boot, RPMB and general-purpose areas by their kernel device names;
the live probe repeats this check after resolving the stable by-id identity.
Existing `mmcblkXpN` GPT partitions are ordinary previous-installation data,
not hardware areas. A main MMC disk uses the same serial selector, partition
roles, fresh-live lifecycle and GRUB installation as other transports. Installed
initramfs is regenerated after final mounts/fstab for both distributions.
Hardware acceptance must verify live detection and installed root boot on the
actual controller; unit tests are not evidence of Zima provisioning success.

Roll out API, operators, UI and site manifests with provisioning idle. Update
homelabd for richer read-only observations. Preserve credentials and existing
installations. Applying layout manifests is non-destructive; validate execution
on disposable hardware/VM before authorizing a physical acceptance run.

Verification covers per-server isolation, optional swap, non-default numbering,
fixed/remaining sizing, stale references, immutable pinned plans, selector
ambiguity, layout conflicts, overflow and physical-layout drift. Unit/syntax
checks alone do not constitute a successful physical installation.
