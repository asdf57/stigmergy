# RFC 0006: kubeadm clusters on managed Servers

- Status: V1 deployed; lab cluster accepted on Zima and Beelink on 2026-10-13 UTC
- Scope: KubeCluster desired state, common Server membership, and separate kube/containerd roles
- Related: RFC 0002 (external operators), RFC 0004 (physical/virtual managed nodes)

## Decision

Use kubeadm, not K3s. Keep two reusable Ansible roles: containerd owns the
runtime; kube owns kubeadm/kubelet installation and init/join configuration.
KubeCluster owns version pins and cluster-wide settings. A Configuration targets
that cluster and a reviewed ordered platform playbook. The shared configuration
operator resolves capture inventory and credentials, invokes that playbook
and reports Configuration status. See RFC 0008 for execution and dependencies.
The playbook selects cluster members from captured Server labels and validates
their roles and installed cluster identities. Roles receive concrete inputs;
they do not query Stigmergy or select resources. Configuration spec references
use names; dynamically resolved UIDs and the Git commit are recorded in status.

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

Readiness belongs to the platform Configuration, not a competing KubeCluster
bootstrap controller. KubeCluster currently has no execution status fields.
Its playbook must verify actual cluster identity, versions, Nodes and networking;
package installation or an open API port is insufficient.

## Membership metadata

Use labels for selection and annotations for non-selector information. Proposed
Server metadata:

```yaml
metadata:
  name: zima
  labels:
    homelab.io/kube-cluster: lab
    homelab.io/kube-role: control-plane
  annotations:
    homelab.io/kube-node-name: zima
```

Role values are control-plane and worker. Default the Kubernetes node name to the
Server name if no override is given. Each Server belongs to at most one cluster;
node names must be unique within that cluster. Validate node-name syntax before
bootstrap. A cluster-name label selects candidates. An optional
homelab.io/kube-cluster-uid annotation can bind membership more strictly; if
supplied, it must match the resolved target. Bootstrap must verify the protected
installed cluster identity rather than adopting a new same-named cluster.
Moving/removing
labels does not automatically reset kubeadm, erase etcd or join another cluster:
report a conflict until an explicit membership-removal/reset workflow exists.

Generic InventoryCaptureGroups select Server kind and these labels and may group
control-plane/workers through their existing selectors. Do not add Kubernetes
fields to the generic capture-group schema. The operator reads Server metadata
from the API, then resolves captured Ansible inventory including inherited vars;
labels/annotations must not be assumed to already exist as Ansible hostvars.

## Execution and role boundaries

1. Load the selected KubeCluster and its exact version/settings inputs.
2. In the platform playbook, select its labeled Servers and verify cluster UID,
   role and unique node name. The generic executor does not interpret these labels.
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
The external operator owns resource lookup, credentials, operation claims and
status. The platform playbook owns membership selection and bootstrap ordering.
Roles own host mutations and remain reusable.

Implemented infrastructure: KubeCluster/Configuration API schemas and generated
endpoints, operators/configurations.py, shared scheduled/manual configuration
pipeline and generic UI support. Cluster bootstrap/platform ordering belongs to
one reviewed playbook; downstream setups may depend on that Configuration.
There is no separate kube_clusters operator or per-Server pipeline.
The standalone containerd play does not implement API locking. The platform
playbook calls its verified-artifact entrypoint under the Configuration executor's
Server claims; roles receive concrete inputs and do not perform API lookup.

## Kubernetes implementation stages

The Configuration executor remains the only generic reconciliation loop. It
resolves named resources and a Git commit, checks dependencies and installed-host
identities, acquires captured Server claims, executes the reviewed playbook and
reports Configuration status. There is no second Kubernetes API controller
executing init/join or competing with that loop.

Implemented V1 path:

- filter_plugins/kube_plan.py contains pure, typed membership/settings/network
  validation. It performs no API, SSH or filesystem operations.
- roles/kube/tasks/plan.yml selects labeled members from configuration_nodes and creates
  execution-local member/control-plane/worker groups. Exactly one control plane
  is supported. Unique node names and optional UID bindings are validated.
- plays/inspect_kube.yml runs planning followed by the read-only host preflight
  in roles/kube/tasks/preflight.yml. It checks installed OS, connected IPv4 CIDRs,
  direct single-control-plane DNS and existing installation artifacts.
- roles/kube/templates/kubeadm-init.yml.j2 supplies v1beta4 init/cluster settings,
  explicit CRI socket, node identity, CIDRs and systemd cgroups.

The inspection playbook is not configure_kube.yml and cannot satisfy platform
readiness. No node setup is executed merely because these source files exist.

The ordered configure_kube.yml path preflights ALL selected
members; resolve and verify exact runtime/Kubernetes/CNI artifacts before any
installation; reconcile containerd without restarting active workloads on
unchanged passes; prepare host networking/swap and kubelet; init the selected
control plane only when verified uninitialized; install pinned Flannel using
the cluster pod CIDR; join workers with short-lived credentials; verify actual
cluster UID, versions, Node roles/Ready, CoreDNS and cross-node pod networking.
Persist protected node/cluster identity and refuse conflicting existing state.
An incomplete init/join must be diagnosed, not retried through an automatic reset.
All setup tasks remain in the two core roles: kube and containerd.

Kubernetes, containerd, runc and base CNI plugins use exact upstream releases
with upstream SHA-256 verification. The pinned Flannel manifest is verified
against its release asset digest and transformed only for ownership and pod CIDR.
No distro package version fallback is used. Native systemd units and CRI/kubelet
systemd cgroups are configured; NetworkManager ignores CNI-created interfaces.
The NoSwap policy disables swap use persistently without altering disk partitions.

The root-owned 0600 node marker records Server/cluster identity, settings, phase,
verified binary hashes, actual installed kube-system namespace UID and the applied
playbook commit. Matching completed installations reuse verified bytes; drift or
new code resolves artifacts again. Running consumers prevent disruptive changes.
Owned CNI interfaces are excluded from overlap checks only after marker validation.

Init is recorded before execution and never replayed against existing admin.conf.
New workers use a ten-minute token and pinned CA discovery; the token is revoked
and join files removed in always cleanup. Credentials remain transient/private,
not API spec, status or inventory. Node admin kubeconfigs remain root-only.
Protected identity plus Configuration.status.outputs.clusterNamespaceUID prevents
a lost control plane from silently becoming a new cluster under the same identity.

Ready requires actual API and kubelet/runtime versions, exact owned membership,
roles/addresses/pod subnets, Ready Nodes, Flannel and CoreDNS. Temporary bounded,
unprivileged, service-account-token-free pods test service DNS and cross-node HTTP.
Only owned probes are cleaned up. Public results are written to the execution
workspace's result.json and reported through generic Configuration.status.outputs.
Timeout or unreachable SSH retains Server claims when remote work is uncertain.

For lab, labels select Zima as control-plane and Beelink as worker. The planned
kube-api.lab.ryuugu.dev endpoint needs DNS pointing to Zima before bootstrap.
The endpoint needs a stable address/reservation and direct DNS, not an HTTP reverse
proxy. See docs/kubernetes-rollout.md. The site pins containerd 2.4.1, runc 1.5.2
and CNI plugins 1.9.1 explicitly. Initial supported hosts are installed Debian/Arch
Linux amd64/arm64 with one control plane, IPv4 and Flannel VXLAN.

## First implementation and acceptance

Source tests cover pure invariants, real localhost Ansible grouping/templates and
guard fixtures. They do not configure managed nodes or prove a live cluster.
Deploy and accept the ordered platform playbook through the Configuration executor.
Keep cluster bootstrap separate from OS
provisioning and VM creation. No existing node is implicitly selected or modified.

Test duplicate node names, replaced cluster/Server UIDs, wrong-cluster labels,
missing pinned packages, active provisioning/operation conflicts, interrupted
init/join, existing-cluster detection and idempotent re-runs. Verify live CRI,
cluster identity, Nodes and workload networking; API declarations and successful
pipeline triggers alone are insufficient. No reset/etcd deletion or node
reprovisioning is authorized by this RFC.

