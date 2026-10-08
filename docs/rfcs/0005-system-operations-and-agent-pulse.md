# RFC 0005: Command-backed system operations and agent reporting pulse

- Status: Implemented in source; deployment and real reboot acceptance pending
- Created: 2026-10-08
- Related: RFC 0002 (external operators), RFC 0003 (ProvisioningRun)
- Scope: reusable reviewed reboot operation and independent homelabd freshness UI

## Decision

Use ordinary disposable Command resources and existing CommandsPipeline runners
for system operations. Do not add Reboot resources, per-Server pipelines or a
Server action endpoint. Managed-node mutations stay in Ansible.

The UI exposes Reboot on Server list/detail views. It requires explicit
Server-name confirmation, chooses a current Ready CommandsPipeline whose capture
group contains the Server, and creates one immutable Command. The script invokes
the reviewed system-operation helper with Server name/UID and bound Machine UID.
It does not contain an arbitrary remote SSH command.

There are no new resource kinds or custom auth mechanisms in this design.

## Execution contract

The existing runner initializes its API credentials, inherited inventory, SSH
user certificate and strict host trust. operators/system_operations.py then:

1. Reads the executor's current InventoryCaptureGroup and resolves inherited
   host variables through ansible-inventory.
2. Requires the explicitly selected Server in that capture group.
3. Checks the current Server UID and Machine UID, deletion, paused state,
   maintenance and active ProvisioningRun ownership.
4. Validates the API-managed public SSH identity/fingerprint and writes a
   private one-host inventory. Replaces transport fields with the current
   management IP, fixed ansible login and strict UID-bound HostKeyAlias.
5. Rechecks identity/address/ownership after preparation.
6. Calls plays/reboot.yml, which includes roles/system_operations/tasks/reboot.yml.

The role independently requires exactly one host and the selected identity
variables. ansible.builtin.reboot records the old boot identity, issues the
reboot, reconnects and verifies a changed boot identity. A fresh SSH connection
must accept the managed Server host key. Existing runner certificate credentials
are reused; no Secrets or private keys enter the Command or logs.

Ansible output streams to the ordinary Command build. Existing Command
Pending/Dispatching/Running/Succeeded/Failed status and build data are the
execution record. The button links to the Command and polls its status while
mounted. Do not invent stored reboot checkpoints or infer success from
heartbeat disappearance/reappearance.

This is a reviewed command path, not a permission boundary against an
administrator who can submit arbitrary Command scripts. Existing broad v1
credentials remain unchanged.

## Lifecycle coordination

The UI and helper refuse a Server reserved by provisioning. Existing Command
dispatch pauses during provisioning maintenance, and the provisioning operator
drains Dispatching/Running Commands before node mutations. If a reservation
appears after the helper's final check, provisioning still waits for that
already-dispatched Command to finish before reboot/erase.

The existing executor serializes its own Command builds. This does not globally
serialize arbitrary jobs or different CommandsPipelines. Administrators must not
start conflicting out-of-band operations. No new global lock/lease framework
is introduced for v1.

A failed/uncertain create or execution must be inspected through Commands; the
UI never silently creates a second attempt. A page reload does not cancel a
running Command. Opening a dialog or polling writes nothing.

## Generic reporting pulse

homelabd already submits MachineReports; no special reboot heartbeat endpoint
or agent release is required. The default agent interval is 30 seconds and the
packaged service currently configures a shorter interval.

Machine.status.lastSeenTime records the API-assigned creation timestamp of the
latest received valid report for that Machine location. It is monotonic and is
not the node-supplied observed_at timestamp or the controller reconciliation time.
A delayed inventory report still refreshes reporting receipt while retaining
newer inventory. Replaying an older stored report cannot advance the timestamp.

Server binding projects this timestamp into the existing
Server.status.agent.lastSeenTime. No bound Machine/report means no agent
observation; unbinding clears the old machine's projected observation.
The existing reachable flag records receipt, not present network reachability.

The UI derives:
- Reporting: receipt age at most two minutes; subtle pulse.
- Stale: older receipt, with its age.
- Never seen: no receipt for the bound machine.
- Unknown: malformed/future timestamp or an API-refresh error.

Polling is read-only, every ten seconds while the page is visible. Respect
reduced-motion preferences. Timestamp-based freshness ages naturally even with
no new report; no timer-driven status writes are needed.

The tooltip states the last API-received report age. Stale does not prove the
host is off, and Reporting does not prove SSH or workloads are healthy.
Discovery reports and the public agent token are not cryptographic attestation.
Heartbeat freshness is independent of Command execution and reboot success.

## Code ownership

- stigmergy: Machine status schema/generated OpenAPI/model; MachineReport
  receipt projection; Server agent projection.
- ansible-roles: operators/system_operations.py, plays/reboot.yml and
  roles/system_operations/tasks/reboot.yml; add later reviewed operations here.
- stigmergy-web: ServerRebootButton, ServerAgentPulse, pure tested helpers and
  Server list/detail integration.
- homelabd: existing MachineReport submission; no reboot execution code.
- homelab-init: existing CommandsPipeline/capture groups; no new reboot pipeline.

## Verification and rollout

Unit tests cover current capture membership, identity replacement, maintenance,
strict managed-key checking, one-host inventory, overridden transport, conditional
request review, failure without retry and timestamp-derived freshness.
API tests verify receipt-time projection and clearing on unbinding. Run Go/API
generation tests, Python unit tests, YAPF/Pylint, Ansible syntax checks and web
tests/build. These checks never reboot a real machine.

Publish the Ansible helper first so Command runners cloning the configured
revision can find it; then deploy API heartbeat projection and the UI. Existing
runner images fetch the Ansible repository, so no image rebuild is required when
that path and dependencies are already present. Preserve tokens and SSH keys.

Real reboot acceptance requires explicit target approval. Verify changed boot
ID, fresh strict SSH, Command completion and subsequent agent report separately.
Do not trigger a real reboot merely to test the new button.
