# RFC 0008: Ordered configuration reconciliation

- Status: Deployed; Kubernetes setup accepted through the shared executor on 2026-10-13 UTC
- Related: RFC 0002 (external operators), RFC 0003 (provisioning), RFC 0006 (kubeadm)

## Decision

### Interactive inventory-group Kubernetes context

An InventoryCaptureGroup may select an existing KubeCluster as its interactive
default context with `metadata.labels.homelab.io/kube-cluster: lab`. This is a
context association, not cluster creation or membership. Server cluster/role
labels still define membership; other captured Servers remain ordinary hosts.
No Kubernetes-specific fields are added to the generic capture-group schema.

`homelabc run` stays unchanged. Normal-mode profile initialization resolves the
label, requires one captured control-plane Server, verifies its managed SSH
identity, and reads its protected admin kubeconfig over strict SSH. The installed
marker must match the Server UID and KubeCluster UID; the actual Kubernetes
namespace and identity ConfigMap must agree. Only a self-contained TLS client
certificate context for the specified endpoint is accepted, without executable
credential plugins or TLS bypasses. The matching kubectl release is fetched over
HTTPS and checksum-verified. Credentials remain private in the disposable
container; neither API status nor logs carry kubeconfig. Refresh discards the
previous generated context and refuses changed lifetimes or unmanaged files.

Command/operator modes never retrieve this admin context. Unlabeled groups
remain unchanged. Interactive cluster-admin credentials are the intentionally
simple learning-stage policy; finer Kubernetes RBAC is future work.

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
  targetRef: {kind: KubeCluster, name: lab}
  inventoryCaptureGroupRef: {name: ssh-managed}
  playbook:
    repositoryRef: {name: ansible-roles}
    path: plays/lab-platform.yml
  values: {}
---
apiVersion: homelab.io/v1alpha1
kind: Configuration
metadata:
  name: lab-applications
spec:
  targetRef: {kind: KubeCluster, name: lab}
  inventoryCaptureGroupRef: {name: ssh-managed}
  playbook:
    repositoryRef: {name: ansible-roles}
    path: plays/lab-applications.yml
  dependsOn:
    - {name: lab-platform}
```

Server targets use targetRef.kind: Server. The target supplies context, not an
implicit readiness dependency or host selector. dependsOn explicitly orders
setups. Desired references use unique names; the operator resolves UIDs and
generations and records them in status. Recreating a reference invalidates old
success and requires fresh validation. suspended stops application without uninstalling.
Deletion also leaves installed software intact in v1.

The playbook is an existing, reviewed file, not inline shell/YAML from the API.
The repository reference must match the operator Git input's origin URL. The
optional revision pins a full commit. Otherwise GitRepository.spec.ref (default
main) resolves from local branch/tag refs in that input. The resolved immutable
commit is recorded in status and included in inputRevision. Missing refs/history
fail visibly, never fall back to another ref or fetch arbitrary repositories.
Extraction rejects unsafe archive paths and playbooks
outside the plays directory. There is one shared repository input in v1, not
arbitrary fetching of repositories using privileged runner credentials.

Public values are passed under configuration_values. Target settings are passed
under configuration_target; captured Servers and their labels/annotations under
configuration_nodes. Values do
not become top-level Ansible variables and cannot override scoped SSH/inventory
settings. Credentials are supplied by the existing private runner context or
reviewed secret-store tasks, never by inline values or status. Configuration
operators receive no new API Secret-read grant.

## Cluster membership

KubeCluster specifies exact Kubernetes/containerd versions, endpoint, pod and
service CIDRs and explicit network provider/version. Server labels select the
cluster and control-plane/worker role. An optional homelab.io/kube-cluster-uid
annotation binds the cluster lifetime; homelab.io/kube-node-name optionally
overrides the Server name. Protected installed identity must be checked before
bootstrap/adoption; desired names alone do not prove the installed cluster.
V1 requires exactly one control plane and unique valid Kubernetes node names.

The executor preserves capture inventory groups and resolved inherited variables.
It does not interpret cluster membership, require a control plane or synthesize
Kubernetes groups. The reviewed platform playbook selects members from the
captured Servers' labels, validates membership/identity, and builds its own
control_plane/worker groups. The complete platform remains one playbook.
All remote mutation targets must stay within the capture group's managed hosts.

A kubeadm role must detect existing cluster/node identity, avoid replaying init
or reset, resolve exact package versions, handle short-lived join credentials
with no_log and verify Nodes/networking. Runtime/cluster upgrades, draining,
etcd deletion and membership removal require explicit safe workflows. A generic
Configuration does not authorize destructive cluster recovery.

## One reconciliation pass

1. Read Configurations selected by the pipeline's capture-group name.
2. Visit dependencies before dependents. Cycles fail before execution; unrelated
   components can still be examined. Missing references or old verification of
   recreated resources never satisfy dependencies.
3. Resolve each target and complete current capture inventory. Omitted selected
   resources defer the setup rather than silently shrinking its membership.
   Require reciprocal
   Server/Machine binding, a successful installation for the same Server UID,
   no active provisioning, an unpaused Server and strict verified SSH identity.
4. A prerequisite must be Ready for its current generation and resolved inputs.
   An old green result is insufficient after a spec, membership or installation
   change.
5. Resolve and extract the playbook commit. Preserve the capture inventory and
   protect its hosts' management transport; the playbook selects execution hosts.
   Acquire existing Server operation claims in deterministic order. Partial claim
   acquisition releases only this pass's already-acquired claims.
   Reread each Server immediately before its claim: heartbeat-only resource
   revisions are refreshed, but changed execution inputs or SSH identity refuse
   execution. A stale pre-checkout snapshot is not used as the claim revision.
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

Timeouts and unreachable SSH retain claims when remote work may remain. Worker crashes also retain
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

Status contains phase, observedGeneration, Ready condition, reason, inputRevision,
playbookRevision, resolvedReferences (kind/name/UID/generation) and lastVerifiedAt.
A buildURL may be supplied by the execution environment.
Failed or pending reconciliation never reports Ready. Status writes are guarded
against same-name recreation and concurrent desired-generation changes.

Reviewed playbooks may publish a public JSON object in configuration_workspace/result.json.
The executor rejects symlinks, non-regular files and results over 64 KiB, and reports
it as status.outputs only after successful guarded execution and input revalidation.
Previous verified outputs are supplied as configuration_observed; private credentials
must never enter these files. This allows domain roles to retain observed identities
without adding a second controller or custom reporting endpoint.

## Implementation and rollout

- stigmergy: generated Configuration/KubeCluster CRUD/status/OpenAPI and the
  token-preserving runner policy preparation.
- ansible-roles/operators/configurations.py: resolution, dependency checking,
  immutable checkout, claims, execution and status.
- plays/configuration_guard.yml: protected installed-host attestation.
- plays/configure_containerd.yml: concrete public-input adapter to the existing
  containerd role. Exact package/version inputs are mandatory.
- plays/inspect_kube.yml and roles/kube/tasks/plan.yml: read-only Kubernetes planning and
  preflight, not platform installation or readiness. See RFC 0006 for staged work.
- homelab-init: shared pipeline and ansible-roles GitRepository declaration.
  No Configuration, cluster membership or Kubernetes installation is applied
  automatically by these site declarations.
- stigmergy-web: generic creation/editing, target/dependency reference links,
  status/input-revision/verification observations.

Before enabling the pipeline, deploy the generated API and prepare its runner
policy from the existing policy, preserving all tokens and Git keys. Apply only
the new GitRepository/Pipeline manifests through the normal conditional/admin
workflow; applying executable Configuration resources is separate approval.
Use name-based references and either a reviewed repository ref or an optional
full commit pin. The pipeline Git input must contain that ref/commit. Start with
an explicitly approved installed capture group; the executor validates and
locks every captured host, even if the playbook uses only a subset.

plays/configure_kube.yml implements the single-control-plane kubeadm/Flannel
platform under this executor; see RFC 0006 and the Kubernetes rollout guide.
Live acceptance remains separate from source tests. Additional application roles
and disruptive upgrades/reset/removal workflows require explicit implementation;
ordinary reconciliation never implies permission to erase/reset cluster state.

Test dependency cycles/staleness, UID replacement, reprovisioning invalidation,
heartbeat stability, strict scoped transport, ownership loss, timeout retention,
failed plays and generation races. No test may configure or provision a real node.
