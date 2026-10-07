# Server provisioning rollout

Implementation: RFC 0003. One `provision` job shares the existing
`reconcile-ssh-host-keys-ssh-managed` pipeline and `server-lifecycle` serial group
with its SSH operator job. Both use `InventoryCaptureGroup/ssh-managed`; a
Partial capture group does not block available Servers. No new resource kind,
per-attempt pipeline, command branch, or custom action endpoint is needed.

## Standup

Keep each Server's provisioning disabled until its disk is explicitly approved
and preflight has passed. Review checked-in enabled flags and request counters
before applying site resources: they are executable desired state. Beelink's
replacement has been explicitly approved; its site counter records preparation
retries and must not be reset by a later initialization.

1. Build/publish the changed Stigmergy and Ansible runner code, and rebuild the
   Arch/Debian live images. Images now contain `/etc/homelabd/live-build-id`;
   installation refuses a live session whose immutable build cannot be verified.
   An older, verified Arch live session can first refresh into the pinned Arch
   build using the guarded live-bootstrap playbook; it is not accepted for erasure.
2. Prepare the existing API policy without rotating tokens:

   ```sh
   go run ./cmd/create-api-auth --policy-source /path/to/existing/api-access.json --output-dir .local/api-auth-provisioning
   ```

   This creates a fresh private directory, preserving admin/agent/runner tokens.
   The runner gains GET/list access to Machine, ISO, SSHCertificateAuthority and
   Command, in addition to its existing public SSHKeyPair/Server/inventory reads
   and generic Server status PATCH. It still cannot GET Secrets or alter specs.
   Install the prepared policy through the normal bootstrap env-file workflow
   and restart the API; never print credentials or regenerate the Git SSH key.
3. Run the normal infrastructure initialization with the new Ansible roles.
   It builds iPXE from the pinned upstream v2.0.0 commit in a local Docker build,
   embedding the public API bootstrap URL and a Let's Encrypt ISRG Root X1 trust
   certificate. It publishes `homelab-ipxe.efi` and its SHA-256 file through the
   existing HTTPS boot server. No privileged token is embedded in iPXE itself.
   Set `DISCOVERY_ISO` to the existing discovery ISO name; the site default is
   `arch-rolling-amd64`. If HTTPS uses a different CA, change the iPXE trust build
   explicitly and retest TLS; do not disable validation.
4. Upload the updated shared Pipeline and capture-group configuration. Its timed
   `provision` job polls every five minutes; a manual job trigger also just polls
   desired state and never increments the reprovision counter.
5. Validate on a disposable UEFI VM before authorizing a physical disk. The
   network-isolated `ansible-roles/operators/tests/grub_boot_vm.sh` checks actual
   GRUB one-shot consumption and local-default boot; it is not a full OS-install
   acceptance test. Verify a full fresh install/reinstall and SSH handoff too.

Use the same private runner credentials/setup as the operator and run
`python3 operators/provisioning.py --preflight` to inspect candidate disks and
dependencies without claiming, installing, changing boot selection or rebooting.
Preflight also examines configured disabled Servers. Initial counter 0 reports
existing disk contents as blocked; it never treats existing partitions as fresh.

## Desired state and execution

Use the existing conditional Server spec PATCH/PUT with `If-Match`. PATCH bodies
are spec merge patches, not envelopes containing another `spec` property.

```yaml
provisioning:
  enabled: true
  reprovision: 0
  targetDisk: /dev/disk/by-id/<approved-physical-disk>
```

Counter 0 only installs a blank approved disk. Existing disks, including Beelink,
require a deliberately incremented replacement counter. For a reviewed
replacement, first configure the target while disabled; enabling it makes the
current counter eligible for execution. After a successful installation, request
another replacement by increasing the counter by exactly one. Ordinary OS,
package, ISO, label or CA edits never authorize a new wipe. Do not roll out an
enabled resource or counter increase as a demonstration: either is real desired
state consumed by the operator.

The current recipes support amd64 UEFI, Arch rolling from Arch live, and Debian
trixie from compatible Arch/Debian live. Secure Boot must already be off for this
unsigned GRUB/iPXE artifact; the operator never changes firmware settings.
The explicit capture-group storage layout is EFI, swap, ext4 root; v1 rejects
other layouts, custom install sources/locales and unimplemented enabled features
before erasure. It applies ordinary users/public key references, packages,
groups, hostname, timezone, validated kernel arguments and sysctls. Root password
login is locked; no extra user credentials are generated or uploaded to Vault.

The operator pins non-secret inputs, physical serial/WWN/size, NIC MAC, dependency
UIDs, ISO build/artifacts and code revision in Server status. It reserves
maintenance, drains administrative builds, and either uses a verified existing
live session or primes the selected NIC and arms `grub-reboot homelab-netboot`.
GRUB normally boots the installed OS locally; API/network availability is only
needed for the explicitly selected netboot path.

For an older USB-booted Arch live session without disk GRUB, the operator can
bootstrap the pinned Arch kernel/initramfs with kexec instead. Ansible verifies
the same session/disk, primes the NIC, downloads checksum-verified HTTPS
artifacts into `/run`, loads the kernel, and requests a systemd kexec only after
the durable AwaitingLive checkpoint. This does not partition or mount the SSD.
The replacement live boot must pass the same build/session/SSH/disk checks before
installation. Unsupported live refresh or unavailable kexec blocks safely;
manual live boot remains a recovery option. Normal installed reprovisioning
continues to use GRUB, not kexec. All reboot requests run through the stage playbook.
Arch netboot arguments include `net.ifnames=0` and a MAC-selected `BOOTIF`, matching
the [upstream Arch netboot script](https://ipxe.archlinux.org/releng/netboot/archlinux.ipxe).
The live root filesystem is downloaded into RAM; a 2 GiB test guest is insufficient
for the current image. Use at least 4 GiB for live-boot tests and budget more for
package installation. Check actual hardware capacity before authorizing installation.

After authenticated live verification, the attempt-scoped TOFU handoff persists
the bootstrap host-key pin before host-private-key delivery. The stable managed
SSH identity, CA trust, fixed `ansible` account reconciler and enrolled daemon
are preserved to the replacement root. The role rebuilds disk GRUB and its iPXE
entry, clears `next_entry`, validates the new UEFI partition/default boot entry,
and writes a root-owned `/var/lib/homelab/provisioning.json` marker.

Only a changed installed boot session, matching marker/root/disk/OS, restored
GRUB, strict managed SSH and healthy management services produce `Succeeded`
and advance `observedReprovision`. Partial or failed execution does not.

## Coordination and recovery

The v1 maintenance gate is intentionally conservative: any provisioning
reservation pauses dispatch of all API-managed Commands, across all capture
groups. Already Dispatching/Running Commands must drain before reboot/erasure.
The normal Server runner also refuses reserved targets. Pending Commands wait.
SSH and provisioning operator builds share a Concourse serial group.
Both operators stream Ansible's standard task/result/recap output to Concourse,
with immediate stage/checkpoint messages. Credential-bearing tasks use Ansible
`no_log`; verbosity and argument display stay off. Private task-container logs
are also retained under `/tmp/provision-operator-diagnostics` or
`/tmp/ssh-host-operator-diagnostics` with directory mode 0700 and file mode 0600.
This is not a universal lock on root/admin access: do not directly trigger raw
command jobs, run unmanaged SSH commands or start another provisioning worker
outside the documented execution path during maintenance.

A preparation failure clears and verifies pending GRUB selection if the original
installed session is still reachable. Uncertain cleanup retains maintenance.
Pause/disable is checked before new stages; completed installation may make its
safe final reboot, while paused post-configuration waits for resume.

Interrupted `Installing` becomes `Blocked`, retaining maintenance, and never
automatically reexecutes installation. Inspect/repair manually, then explicitly
clear the verified maintenance/boot selection through generic status PATCH and
increment the request only if another destructive attempt is intended.
For a verified partial root with its original live session and chroot mounts
still present, the explicit `repair` stage runs configuration only: it verifies
the pinned boot/build/disk, `/mnt` root and EFI mount sources, and bind mounts,
then completes management, GRUB and the staged marker without partitioning,
formatting or bootstrapping packages again. Record the repair code revision in
the checkpoint message. Only after checking the completed staged marker may an
explicit generic status update move that same Blocked attempt to
`AwaitingInstalled`, retaining maintenance and the original live boot ID.
It cannot return to `Installing`; fresh installed-boot verification still gates
success. Resume the final verification with the pinned original operator revision.
`AwaitingInstalled`/`Verifying` resumes marker-based final boot/verification,
not partitioning. Resume uses the pinned operator-code revision; inspect and
select that Git resource version in Concourse if the source branch moved.

If GRUB is destroyed, the disk fails, or power is lost before GRUB is restored,
boot a live ISO manually and inspect the disk/checkpoint. No firmware PXE-first,
permanent USB, protected bootstrap partition, PiKVM, or automatic recovery is
required in v1. Manual recovery itself is not permission for another wipe.
