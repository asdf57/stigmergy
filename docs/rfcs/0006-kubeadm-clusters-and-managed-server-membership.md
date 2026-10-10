# RFC 0006: kubeadm clusters on managed Servers

- Status: Draft; containerd role is implemented, cluster resources/operator are proposed
- Scope: KubeCluster desired state, common Server membership, and separate kube/containerd roles
- Related: RFC 0002 (external operators), RFC 0004 (physical/virtual managed nodes)

## Decision

Use kubeadm, not K3s. Keep two reusable Ansible roles: containerd owns the
runtime; kube owns kubeadm/kubelet installation and init/join configuration.
KubeCluster owns version pins and cluster-wide settings. A bounded external
operator looks up the selected cluster and its Servers, resolves inventory and
credentials, invokes reviewed plays, and reports generic object status.
Roles receive concrete inputs; they do not query Stigmergy or select resources.

VM-backed and physical Servers are opaque to this workflow. A cluster can contain
Beelink and VM-backed Servers without separate bootstrap or membership logic.
VM hardware creation and OS ProvisioningRuns remain separate prerequisites.
Declaring membership never authorizes OS erasure or reprovisioning.

## Proposed KubeCluster contract

Desired state includes exact Kubernetes and containerd versions, API endpoint,
pod/service CIDRs, and explicit cluster-network choice/settings. Later runtime
settings belong here too, not in duplicated per-Server version labels. Avoid a
second membership list in the cluster spec: Server labels are the desired members.

Version resolution must be explicit. kubeadm, kubelet and any installed kubectl
use the selected Kubernetes version. The operator resolves the runtime version to
an exact supported distro package/artifact and derives the sandbox image from the
selected kubeadm version. Distro package release suffixes are not upstream version
numbers. Missing packages or an unsupported Kubernetes/runtime combination reports
a condition, never a fallback to latest. Runtime and Kubernetes upgrades need a
separate reviewed upgrade sequence; changing a version field is not permission to
restart a busy cluster indiscriminately.

Observed status records observed generation, resolved member Server/Machine UID
references, cluster identity, bootstrap progress, effective versions, Kubernetes
Node identity/readiness and conditions. A green Ansible build or open API port
alone does not prove node membership or a functioning pod network.

## Membership metadata

Use labels for selection and annotations for non-selector information. Proposed
Server metadata (not deployed API conventions yet):

```yaml
metadata:
  name: beelink
  labels:
    homelab.io/kube-cluster: lab
    homelab.io/kube-role: control-plane
  annotations:
    homelab.io/kube-cluster-uid: <KubeCluster UID>
    homelab.io/kube-node-name: beelink
```

Role values are control-plane and worker. Default the Kubernetes node name to the
Server name if no override is given. Each Server belongs to at most one cluster;
node names must be unique within that cluster. Validate node-name syntax before
bootstrap. A cluster-name label selects candidates; the UID binding prevents a
new cluster with the same name from silently adopting old nodes. Moving/removing
labels does not automatically reset kubeadm, erase etcd or join another cluster:
report a conflict until an explicit membership-removal/reset workflow exists.

Generic InventoryCaptureGroups select Server kind and these labels and may group
control-plane/workers through their existing selectors. Do not add Kubernetes
fields to the generic capture-group schema. The operator reads Server metadata
from the API, then resolves captured Ansible inventory including inherited vars;
labels/annotations must not be assumed to already exist as Ansible hostvars.

## Execution and role boundaries

1. Load the selected KubeCluster and its exact version/settings inputs.
2. Select its labeled Servers and verify cluster UID, role and unique node name.
3. Require installed, bound, reachable Servers with verified management SSH.
   Resolve current capture membership and reject active ProvisioningRuns.
4. Resolve supported distro-specific package pins before mutations. Build scoped
   inventories and acquire existing per-Server operation claims for host changes.
5. Run containerd, then kube host prerequisites/package installation. containerd
   owns CRI configuration, runc/systemd cgroups and service/plugin verification.
   kube owns kubelet configuration, required networking/sysctl/swap policy and
   matching cgroup settings. Cluster networking is explicit, not implicit runtime setup.
6. For the initial implementation, bootstrap one explicitly selected control-plane
   with kubeadm init, persist the observed cluster identity, then join approved
   workers. Additional control planes/HA require a later reviewed extension.
   Never replay kubeadm init against an existing cluster or auto-reset a failed node.
7. Install the selected cluster network and verify actual Kubernetes Nodes, version,
   role and readiness. Report per-node progress and cluster conditions through status.

Join credentials and kubeconfigs belong in existing Secret/SecretStore resources,
not annotations, inventory dumps or logs. Fetch/generate scoped, short-lived join
credentials as needed and use no_log only for credential-bearing Ansible tasks.
The external operator owns resource lookup, credentials, operation claims,
bootstrap ordering and status. Roles own host mutations and remain reusable.

Planned code: new API resource schema/generated endpoints in stigmergy;
operators/kube_clusters.py and reviewed cluster plays in ansible-roles;
KubeCluster/Server metadata, capture groups and one shared scheduled/manual
operator pipeline in homelab-init. No pipeline per Server or parallel VM workflow.
The current containerd role and explicit-target play do not implement API locks
or cluster lookup and are not wired to automatic node reconciliation.

## First implementation and acceptance

First build/test the independent containerd role without changing managed nodes.
Next implement KubeCluster validation/resource generation, member selection and
a single-control-plane kubeadm operator. Keep cluster bootstrap separate from OS
provisioning and VM creation. No existing node is implicitly selected or modified.

Test duplicate node names, replaced cluster/Server UIDs, wrong-cluster labels,
missing pinned packages, active provisioning/operation conflicts, interrupted
init/join, existing-cluster detection and idempotent re-runs. Verify live CRI,
cluster identity, Nodes and workload networking; API declarations and successful
pipeline triggers alone are insufficient. No reset/etcd deletion or node
reprovisioning is authorized by this RFC.

