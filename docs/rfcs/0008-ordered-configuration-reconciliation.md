# RFC 0008: Ordered configuration reconciliation

- Status: Implemented in source; rollout and managed-node acceptance pending
- Related: RFC 0002 (external operators), RFC 0003 (provisioning), RFC 0006 (kubeadm)

## Decision

A Configuration represents one coherent setup, not one role or disposable job.
Its reviewed Ansible playbook orders roles locally. dependsOn references order
separate setups. ProvisioningRun authorizes OS replacement; Configuration never
does. Physical and VM-backed Servers use the same target contract.

KubeCluster stores cluster settings and Server membership conventions. It has
no separate bootstrap controller or competing installation pipeline. A platform
Configuration targeting it can run containerd, kubeadm, initial CNI, joins and
platform roles in a human-readable playbook. Subsequent application configurations
depend on the platform Configuration, not on a circular cluster Ready flag.

Configuration.status is the execution/readiness authority. KubeCluster currently
has no executable status fields. Declaring either resource alone does not prove
a functioning cluster.

## Resource example

```yaml
apiVersion: homelab.io/v1alpha1
kind: Configuration
metadata:
  name: lab-platform
spec:
  targetRef: {kind: KubeCluster, name: lab, uid: <cluster-uid>}
  inventoryCaptureGroupRef: {name: ssh-managed, uid: <group-uid>}
  playbook:
    repositoryRef: {name: ansible-roles, uid: <repository-uid>}
    revision: <full-reviewed-40-character-git-commit>
    path: plays/lab-platform.yml
  values: {}
---
apiVersion: homelab.io/v1alpha1
kind: Configuration
metadata:
  name: lab-applications
spec:
  targetRef: {kind: KubeCluster, name: lab, uid: <cluster-uid>}
  inventoryCaptureGroupRef: {name: ssh-managed, uid: <group-uid>}
  playbook:
    repositoryRef: {name: ansible-roles, uid: <repository-uid>}
    revision: <full-reviewed-40-character-git-commit>
    path: plays/lab-applications.yml
  dependsOn:
    - {name: lab-platform, uid: <configuration-uid>}
```

Server targets use targetRef.kind: Server. No Kubernetes dependency is implicit
for host services. References pin resource lifetimes; recreating a name never
adopts the replacement silently. suspended stops application without uninstalling.
Deletion also leaves installed software intact in v1.

The playbook is an existing, reviewed file, not inline shell/YAML from the API.
The repository reference must match the operator Git input's origin URL. The
exact commit must be available in that input; missing history fails visibly,
never falls back to main. Extraction rejects unsafe archive paths and playbooks
outside the plays directory. There is one shared repository input in v1, not
arbitrary fetching of repositories using privileged runner credentials.

Public values are passed under configuration_values. Target settings are passed
under configuration_target; cluster nodes under configuration_nodes. Values do
not become top-level Ansible variables and cannot override scoped SSH/inventory
settings. Credentials are supplied by the existing private runner context or
reviewed secret-store tasks, never by inline values or status. Configuration
operators receive no new API Secret-read grant.

## Cluster membership

KubeCluster specifies exact Kubernetes/containerd versions, endpoint, pod and
service CIDRs and explicit network provider/version. Server labels select the
cluster and control-plane/worker role. homelab.io/kube-cluster-uid binds the
cluster lifetime; homelab.io/kube-node-name optionally overrides the Server name.
V1 requires exactly one control plane and unique valid Kubernetes node names.

The executor provides control_plane and worker inventory groups, preserving
resolved inherited capture variables. The complete platform remains one
playbook, even when its plays target different groups or localhost for cluster
API operations. All remote mutation targets must be selected managed members.

A kubeadm role must detect existing cluster/node identity, avoid replaying init
or reset, resolve exact package versions, handle short-lived join credentials
with no_log and verify Nodes/networking. Runtime/cluster upgrades, draining,
etcd deletion and membership removal require explicit safe workflows. A generic
Configuration does not authorize destructive cluster recovery.

## One reconciliation pass

1. Read Configurations selected by the pipeline's capture-group name.
2. Visit dependencies before dependents. Cycles fail before execution; unrelated
   components can still be examined. Missing/replaced references never satisfy
   dependencies.
3. Resolve each target and current capture inventory. Require reciprocal
   Server/Machine binding, a successful installation for the same Server UID,
   no active provisioning, an unpaused Server and strict verified SSH identity.
4. A prerequisite must be Ready for its current generation and resolved inputs.
   An old green result is insufficient after a spec, membership or installation
   change.
5. Extract the pinned playbook and scope inventory/credentials to selected hosts.
   Acquire existing Server operation claims in deterministic order. Partial claim
   acquisition releases only this pass's already-acquired claims.
6. Execute the installed-host guard, ordered playbook and guard again. The guard
   checks the protected Server/run marker and refuses live environments. Reread
   ownership before each play. Stream normal Ansible output; protect credential
   tasks individually.
7. Recompute current relevant inputs. Changed inputs cannot become Ready.
   Release claims only after completed work and report the verified result.

A playbook must include actual component verification, not merely package
installation or an open port. Generic execution cannot invent domain-specific
health checks. Ansible success plus installed identity verification means the
reviewed playbook contract succeeded; it is not unconditional proof of arbitrary
application health.

Timeouts retain claims when remote work may remain. Worker crashes also retain
claims. No automatic expiry, blind cleanup, OS reboot or erasure retry exists.
Inspect the worker and node before conditionally releasing the exact stopped
operation. Normal completed failures release their claims and report failure.

## Scheduling, drift and reprovisioning

The shared reconcile-configurations-ssh-managed/operator Concourse job runs
every five minutes and supports manual triggering. One serial bounded pass
rereads current state; no per-Configuration/per-role pipelines or timer Commands
are created. The API stores desired/observed state and does not run Ansible.

Eligible playbooks rerun idempotently each pass, even without a generation change,
to inspect and repair safe drift. Only the role/playbook can determine whether a
particular component needs changes. Disruptive upgrades must not be hidden in
ordinary periodic reconciliation.

inputRevision is a fingerprint of relevant desired inputs, target identities,
member bindings, successful installation UIDs, repository settings and dependency
input revisions. Heartbeat timestamps and operation IDs are excluded. It is not
an execution request or a substitute for physical verification. A new successful
ProvisioningRun changes this fingerprint even when Configuration and Server
generations remain unchanged. Dependents therefore wait for fresh verification.
A normal host reboot does not create a new installation or authorize rejoining
another cluster.

Status contains phase, observedGeneration, Ready condition, reason, inputRevision
and lastVerifiedAt. A buildURL may be supplied by the execution environment.
Failed or pending reconciliation never reports Ready. Status writes are guarded
against same-name recreation and concurrent desired-generation changes.

## Implementation and rollout

- stigmergy: generated Configuration/KubeCluster CRUD/status/OpenAPI and the
  token-preserving runner policy preparation.
- ansible-roles/operators/configurations.py: resolution, dependency checking,
  immutable checkout, claims, execution and status.
- plays/configuration_guard.yml: protected installed-host attestation.
- plays/configure_containerd.yml: concrete public-input adapter to the existing
  containerd role. Exact package/version inputs are mandatory.
- homelab-init: shared pipeline and ansible-roles GitRepository declaration.
  No Configuration, cluster membership or Kubernetes installation is applied
  automatically by these site declarations.
- stigmergy-web: generic creation/editing, target/dependency reference links,
  status/input-revision/verification observations.

Before enabling the pipeline, deploy the generated API and prepare its runner
policy from the existing policy, preserving all tokens and Git keys. Apply only
the new GitRepository/Pipeline manifests through the normal conditional/admin
workflow; applying executable Configuration resources is separate approval.
Pin a commit containing the chosen reviewed playbook and use actual reference
UIDs. Start with an explicitly approved installed test target.

This implementation provides the generic configuration executor and cluster
input model. The existing kube role is not a completed kubeadm implementation;
a production lab-platform playbook, kubeadm init/join/upgrade roles and Kubernetes
application roles require their own implementation and acceptance. Do not point
a Configuration at an unverified bootstrap play or claim a cluster was installed.

Test dependency cycles/staleness, UID replacement, reprovisioning invalidation,
heartbeat stability, strict scoped transport, ownership loss, timeout retention,
failed plays and generation races. No test may configure or provision a real node.
