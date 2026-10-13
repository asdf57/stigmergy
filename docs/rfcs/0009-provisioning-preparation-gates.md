# RFC 0009: Provisioning preparation gates

- Status: Proposed for later; not implemented
- Related: RFC 0003 (ProvisioningRun), RFC 0006 (Kubernetes), RFC 0008 (Configuration)

## Problem and decision

Provisioning replaces an OS, but an installed node may first need application
cleanup: drain Kubernetes workloads, remove its Node, or leave another managed
service. Configuration currently installs/reconciles software; it does not
decommission nodes. Current ProvisioningRun execution has no application cleanup
barrier. Do not describe ordinary reprovisioning as safe Kubernetes draining.

Use a finalizer-like preparation barrier on the existing ProvisioningRun,
not another disposable request resource. Actual finalizers retain their deletion
meaning. Preparation gates control when execution may begin, not when an object
may be deleted. The normal fresh-live-image lifecycle remains unchanged after
preparation; gates never replace disk approval or boot/disk identity checks.

## Responsibilities

- Configuration declares a reviewed beforeProvision playbook for its coherent
  setup. It uses the same trusted repository/immutable commit and public context
  contracts as normal reconciliation, not inline privileged commands.
- The API resolves applicable Configurations and records every required gate
  before the run can advance. Request callers cannot omit or remove required
  cleanup. Missing/ambiguous handlers fail closed.
- The existing external Configuration/Ansible machinery executes cleanup and
  reports generic run status. There is no second API Kubernetes bootstrap
  controller, per-node pipeline or agent-side disk/service teardown.
- The provisioning operator waits for all required gates, then follows RFC 0003.

## Recorded preparation plan

The exact schema is deferred. Record a fixed plan tied to the run UID, Server
and Machine UIDs, the installed setup identity, each Configuration UID/generation,
resolved cleanup playbook commit and relevant inputs. Names are desired refs;
resolved identities are recorded dynamically, not copied into site manifests.

Each gate has a stable ID, handler reference, Pending/Running/Ready/Failed phase
and useful public reason/message/build observations. A completion for an older
run, installation, Configuration or cluster lifetime cannot satisfy this plan.
An empty plan is valid only when resolution proves no managed cleanup is needed.
Do not allow gates to be discovered after boot/erasure has already begun.

## Execution and ownership

1. Creation reserves the Server as today and pins the preparation plan.
2. Normal Configuration reconciliation defers while that run owns the node.
3. Execute each required cleanup through reviewed Ansible, in explicit dependency
   order when handlers require it. Keep independent cleanup/checkpoint progress
   visible to humans. No generic workflow engine is required for the first case.
4. Cleanup executes under the run's reservation with an explicit UID-bound
   delegation/ownership check. It must not try to acquire a conflicting ordinary
   Server operation claim or open a gap by releasing the run reservation.
5. Report Ready only after actual cleanup verification for the pinned identity.
6. Provisioning may prepare/reboot only after every required gate is Ready.
   It must still verify a fresh pinned live boot before any approved disk erasure.
7. After successful installed verification, normal Configuration reconciliation
   may install/rejoin the node according to its desired membership labels.

Failed gates block visibly. Retrying cleanup must be idempotent and identity-bound;
it is not an automatic erasure retry. Unreachable/crashed workers require inspection
of uncertain remote work. There is no automatic gate expiry, force bypass or
forced eviction. Deleting a request is not cancellation of remote work; preserve
the existing safe run-deletion/ownership rules and define preparation-phase
cancellation before allowing deletion of active cleanup.

## First handler: Kubernetes

The cluster playbook supplies a decommission entry point. Verify the protected
node/cluster identity and actual Kubernetes Node UID, cordon/drain respecting
PodDisruptionBudgets, verify workload evacuation, then remove that exact Node.
Capture enough public observations for an interrupted pass to resume safely.
Do not infer completion merely because a node or API endpoint is unavailable.

Ordinary reprovisioning of the sole control plane is refused. Losing its disk
destroys the cluster; explicit cluster destruction/rebuild or a future safe HA
control-plane removal workflow is separate authorization. Membership removal
without OS reprovisioning is also a separate operation, not an implicit wipe.

## Implementation work for a later phase

- stigmergy: generic preparation plan/status schema, required-gate resolution,
  state-transition validation and cancellation/deletion boundaries.
- ansible-roles: Configuration cleanup entry points, run-bound execution ownership,
  Kubernetes drain/remove verification and the provisioning wait barrier.
- homelab-init: reviewed cleanup declarations alongside the existing setup.
- stigmergy-web: preparation gate/checkpoint progress and explicit blocked reasons.
- Tests: omitted handlers, stale completions, concurrent reconciliation, dependency
  order, disruption budgets, interrupted drain, worker loss, single-control-plane
  refusal and proof that no boot/wipe occurs before preparation is complete.

This RFC records the agreed direction only. Do not silently introduce these
fields into the current API or weaken the existing provisioning lifecycle.
