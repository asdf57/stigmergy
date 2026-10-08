# ProvisioningRun standup and execution

The shared reconcile-ssh-host-keys-ssh-managed pipeline has operator and provision
jobs sharing server-lifecycle and InventoryCaptureGroup/ssh-managed. Scheduled
or manual triggers poll requests; they never authorize disk replacement.

## Standup

1. Coordinate API, operator and UI rollout only with lifecycle work idle. Preserve
   existing installation observations, Git SSH keys, API tokens and host keys.
   Do not run init or deploy incompatible schemas over an active installation.
2. Generate API models/OpenAPI from the resource modules with make generate.
   Publish the API and reviewed operator revision; update the web console.
3. Prepare runner policy using the existing tokens:

   ```sh
   go run ./cmd/create-api-auth --policy-source /path/to/private/api-access.json --output-dir .local/api-auth-runs
   ```

   Runner can GET Machine, Server, ISO, CA, public SSHKeyPair, capture groups,
   Command and ProvisioningRun, and PATCH Server/run generic status. Admin creates
   requests. Runner cannot fetch API Secrets or create runs. Install the policy
   via the established private env-file/bootstrap workflow; never print it.
4. Configure discovery ISO, Ready Server boot ISO/CA/target OS, management network
   and LLDP selector. Set provisioning.enabled=true to permit explicit runs and
   first-machine live discovery; that flag alone never installs or erases.
5. Build/publish the shared iPXE artifact through plays/build_ipxe.yml and immutable
   live ISO artifacts when their inputs change. iPXE trusts packaged ISRG X1 and
   USERTrust ECC/RSA roots; never bypass TLS. Preserve protected daemon enrollment.
6. Verify discovery, Machine inventory/binding and managed SSH readiness. Test the
   full run lifecycle on disposable hardware/VM before a physical acceptance run.

## Request installation

Use the Server's Provision dialog to select a distribution, version, Ready live
ISO and discovered disk, then type the Server's exact name. Choices come from
current Ready ISO resources with completed builds, filtered to supported amd64
UEFI targets (Arch rolling and Debian trixie). Multiple matching ISOs are explicit
choices; an empty catalog prevents submission. Merely opening or changing the
dialog writes nothing.

Confirmation first conditionally PATCHes the selected OS and UID-qualified boot
ISO into Server spec, preserving other OS settings and packages, then POSTs a
ProvisioningRun with the returned Server generation. Review distribution-specific
packages/settings before switching OS. These writes are not one transaction:
if run creation fails, the desired OS/ISO may remain saved without a confirmed
installation request. Close, refresh and review status before another attempt;
there is no automatic retry or rollback. Operator preflight still verifies current
ISO/CA readiness and all installation dependencies.

Alternatively, configure the desired Server spec and POST a reviewed resource
to /api/v1alpha1/provisioning-runs:

```yaml
apiVersion: homelab.io/v1alpha1
kind: ProvisioningRun
metadata:
  name: node-install-001
spec:
  serverRef: {name: node, uid: <server-uid>}
  serverGeneration: <reviewed-server-generation>
  machineRef: {name: machine, uid: <machine-uid>}
  storage:
    disks:
      - deviceID: wwn:<discovered-wwn>
        role: system
```

Creation explicitly authorizes erasure, even for the first installation. There is
no disk field or replacement counter in Server spec. V1 supports one SATA/ATA/NVMe
system disk with EFI/swap/ext4 layout, amd64 UEFI with Secure Boot already off,
Arch rolling or Debian trixie. USB, removable/read-only/ambiguous disks and
unsupported layouts/features block. Do not check destructive runs into init.

Creation atomically reserves the Server and records discovered disk identity.
Conflicts require a fresh review, not an automatic retry. Specs cannot be changed.
The operator independently verifies serial/WWN/size, stable by-id path, mounts,
boot/session, dependencies and immutable execution inputs before installation.

Run python3 operators/provisioning.py --preflight with the existing runner setup
to inspect pending runs without claiming, rebooting or installing. No run means
no installation. Polling the job without a run is an idle no-op.

## Progress and recovery

Read ProvisioningRun.status for phase, currentStage, message, snapshot, boot IDs,
attemptID, backendRunID and timestamps. Read Server.status.provisioning for the
activeRunRef, maintenance and lastSuccessfulRunRef. The Server detail page follows
the run and refreshes every five seconds; it does not invent checkpoint history.

Standard Ansible output streams to Concourse. Secret tasks use no_log; private
task logs are retained inside the task container. Trace the shared job/build,
not a per-request pipeline. Scheduled idle success does not prove installation.
Blocked runs retaining maintenance make reconciliation fail visibly; they never
silently turn green or replay installation. Run messages retain the safe failing
task description. Validate the real Ansible installation assertions as well as
mocked Python state-machine tests before publishing an operator revision.

Normal boot is local disk GRUB. Replacement arms the one-shot homelab-netboot
entry, primes the current interface selected by pinned MAC and boots the pinned
live ISO. iPXE Permission denied can be certificate validation; the UEFI TLS
fixture tests the actual API/artifact chain. Arch live boot requires BOOTIF and
net.ifnames=0 and at least 4 GiB for the tested RAM image.

Only a changed installed boot, matching run marker/root/disk/OS, restored GRUB,
strict SSH identity and healthy services complete a run. The run is marked
Succeeded before Server reservation release; a later pass can finish release
without reinstalling. Run UID, not a counter, binds the installed marker.

Interrupted Installing becomes Blocked with maintenance retained; never rewipe
automatically. Verify the exact original live boot/build/disk and staged mounts
before using the configuration-only repair stage. After verified staged completion,
the same run can move to AwaitingInstalled with the immutable original snapshot
and liveBootID retained. Resume final verification using its pinned code revision.
See ansible-roles/AGENTS.md for fly pin/watch/hijack recipes.

Preparation failure must clear/read back boot selection before maintenance release.
Blocked runs with verified cleanup can release their reservation; a new installation
requires a new run. Pending cancellation may report Blocked/maintenance=false only
before privileged work, then release the Server. Reserved runs cannot be deleted;
completed unreserved runs may be deleted with If-Match. No automatic TTL exists.

For a verified failure before erasure, the explicit cleanup playbook
plays/verify_failed_provision_cleanup.yml checks the original live session, selected
unmounted SSD, prior installed marker/root and absence of firmware/kexec overrides.
It clears only the old GRUB next_entry and unmounts before maintenance release.
The run remains Blocked history; releasing activeRunRef unlocks the Provision
dialog. Do not clear flags merely because netbootArmed is false. The playbook
does not reboot, erase or create a new request.

Manual console/live-media recovery is accepted when bootloader/disk recovery is
necessary. Never use PiKVM. A successful trigger/build or port 22 opening is not
acceptance: independently inspect the run status and actual installed node.
