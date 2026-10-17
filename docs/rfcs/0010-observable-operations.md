# RFC 0010: Observable exclusive operations

Status: Deployed and verified on 2026-10-16 UTC.

Operation is an immutable coordination record, not a job runner or workflow
engine. Its spec contains an action, operator name, UID-qualified targetRef
(kind/name/uid), and optional UID-qualified requestedBy resource reference.
Any registered resource with a status schema may be a target except Operation
itself. One operation targets one resource; a multi-node Configuration acquires
one per Server before executing its reviewed repository-backed command.

POST atomically creates a Running Operation and sets the target's generic
status.operationRef. Existing ownership, deletion or Server provisioning
maintenance rejects acquisition. The API owns operationRef; ordinary status
writers preserve it. The shared generated status contract exposes the reference
for every status-bearing resource, without Server-specific fields in other kinds.

Status contains phase, startedAt, updatedAt, completedAt, currentStep and a public
message. Running may become Succeeded, Failed or Uncertain. Uncertain retains
ownership; explicit inspected resolution may become Succeeded or Failed. Terminal
completion and clearing the exact target reference are atomic. No timeout,
heartbeat expiry, delete or retry releases uncertain work. Active operations and
their targets cannot be deleted. Terminal records remain until explicitly deleted.
There is no automatic TTL, cancellation, stealing or erasure retry.

Configuration, SSH management and ordinary system-operation executors create
Operations and report bounded progress. Known completed failures become Failed;
transport loss, timeouts and unclassified interruptions become Uncertain.
An abrupt process death may leave Running with an old updatedAt instead;
that observation still retains ownership and requires inspection. Public
messages never contain raw credential-bearing exceptions or Ansible output.
Runner auth allows Operation GET/POST and status PATCH, not resource deletion or
Secret reads. Existing API tokens remain unchanged.

ProvisioningRun remains the immutable disk-installation request and keeps its
full-run Server reservation. Its atomic creation conflicts with operationRef;
Operation creation conflicts with that reservation. Neither bypasses the other.
The web UI links active ownership to its Operation and reports busy ownership
separately from desired-generation/CAS conflicts. Provision/reboot keep explicit
warning dialogs and action buttons, but do not require typing a Server name.

Rollout must drain and pause the existing operators before replacing the claim
contract. Do not clear a retained claim or invent an Operation for ongoing work.
Inspect stranded work first; any one-time rescue is performed by the owner.
Deploy the API/status schema and token-preserving permission update before the
new executors, then the UI. Source and isolated store tests must not provision or
reboot real nodes. Acceptance requires observable operations, exclusive ownership
and verified existing cluster state, not a green trigger alone.

Rollout acceptance: API 50544e8, operators 1918613 and UI 9498492. Configuration
build 900 verified both existing nodes, service DNS and cross-node HTTP, then
completed its two Operations successfully. SSH reconciliation also completed
Operations for both nodes. Target ownership was released on completion; fresh
KubeCluster observations and the served UI were verified. All four execution
jobs are unpaused. No provisioning or reboot request was created.
