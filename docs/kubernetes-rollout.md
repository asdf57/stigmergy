# kubeadm/Flannel cluster rollout

The lab cluster is deployed and accepted: Zima is control-plane and Beelink is
worker. Source tests alone are not live acceptance; each rollout must verify the
actual cluster. No node is configured just by building code.
Applying an enabled Configuration authorizes its reviewed setup, not OS erasure.

## Prerequisites

- Deploy the generated API accepting name-based Configuration refs, optional
  revisions, public outputs and KubeCluster runtime/CNI pins. Preserve existing
  API tokens and Git/SSH credentials. No new authentication scheme is required.
- Publish the reviewed ansible-roles implementation and verify the shared
  reconcile-configurations-ssh-managed/operator pipeline's code input contains
  that commit/ref. No per-cluster pipeline or Kubernetes API bootstrap controller.
- Deploy the typed KubeCluster status schema before publishing its reporting
  operator. Prepare the existing API policy with `create-api-auth --policy-source`
  to add runner KubeCluster status PATCH while preserving every token. Apply the
  same policy to the bootstrap environment and runtime policy file; do not grant
  runner Secret reads. Then deploy the web overview and verify an actual operator
  pass populated KubeCluster/lab.status, not merely a green build.
  The protected runtime policy must be owned by numeric UID/GID `65532:65532`
  with mode `0400`, matching the distroless API user. Do not use the host account
  name `nobody` (UID 65534 on Arch). When replacing a file bind-mounted into the
  API, recreate that container so the mount picks up the installed file.
- Require both Servers to be bound and successfully provisioned into installed
  OSes, with Ready managed SSH and no active provisioning/operation ownership.
  The capture must be current and complete, with no omitted selected resources.
- Reserve a stable control-plane IPv4 address and create direct DNS for
  kube-api.lab.ryuugu.dev pointing to Zima. Do not point it at the HTTP reverse
  proxy: kubeadm owns API TLS and client authentication. Both nodes must resolve
  that name to the selected address. Configure site networking explicitly; do not
  infer a permanent DNS value from an old discovery snapshot.
  The lab uses static DHCP reservations for both managed nodes: Zima
  10.1.1.28 (00:e0:4c:34:97:90), Beelink 10.1.1.243 (e8:ff:1e:d4:03:fa).
  `plays/reserve_router_dhcp.yml` validates one explicitly approved, uniquely
  bound address/MAC pair, makes only that lease static and verifies it. It
  requires the standard community.routeros collection and librouteros; credentials
  are private extra-vars, never source/status. The site DNSRecord/kube-api-lab
  maps the API endpoint directly to Zima. Reserving a lease does not restart
  NetworkManager or alter the current node connection.
- Confirm 172.28.0.0/16 pods and 172.29.0.0/16 services do not overlap LAN/VPN/
  container networks. Host connected-network checks run before setup. Permit
  normal Kubernetes traffic and Flannel UDP 8472 between members.
- V1 supports one control plane, IPv4, Flannel and installed Debian/Arch Linux
  amd64/arm64 hosts. The control plane needs at least two CPUs; nodes need about
  2 GiB RAM and enough free memory to disable occupied swap safely.

## Declare and reconcile

Review homelab-init/KubeCluster/kube-cluster-lab.yaml,
Configuration/configuration-kube-lab.yaml and the two Server label changes.
Cluster pins include Kubernetes 1.37.1, containerd 2.4.1, runc 1.5.2,
CNI plugins 1.9.1 and Flannel 0.28.10. The Configuration targets lab, uses
ssh-managed capture and plays/configure_kube.yml from ansible-roles.

Use the existing admin resource workflow to apply only these reviewed declarations
after explicitly approving node configuration. References use names, not copied
UIDs. Omitted revision resolves GitRepository.ref (default main) from the trusted
pipeline input; an optional full commit pin is supported. Missing code fails
visibly, never falls back or fetches an arbitrary repository.

The shared operator runs every five minutes; a manual trigger polls the same
desired state. It resolves identities/commit, checks dependencies, verifies
installed hosts, holds one generic Operation per Server and runs the ordered playbook. Ansible task
progress streams normally; init/join credential tasks alone are hidden.

Inspect a setup first with the separate plays/inspect_kube.yml through a reviewed
inspection Configuration if desired. Its success is not platform readiness.

## Acceptance

Require Configuration/kube-setup-lab Ready for its current generation and
inputRevision. Inspect playbookRevision, resolvedReferences, lastVerifiedAt and
outputs: clusterUID, actual clusterNamespaceUID, versions and selected Nodes.
Check the exact job logs, not merely a green trigger or stale status.

KubeCluster.status is the public domain report: inspect lastVerifiedAt,
observedGeneration, configurationRef/generation, inputRevision/playbookRevision,
clusterNamespaceUID, workload checks and the actual per-Node observations. The
web cluster overview renders this status separately from Configuration execution
and Server heartbeats. Older timestamps are historical observations, not ongoing
health monitoring. No kubeconfig is stored in object status.

From the initialized management container, use ssh zima and the node's root-only
admin configuration to inspect actual state:

```sh
sudo /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide
sudo /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get pods -A
```

The playbook checks API/kubelet/runtime versions, ownership, roles, addresses and
pod subnets; Flannel/CoreDNS rollout; and DNS/HTTP from temporary pods on every
selected node. Root-owned 0600 node markers persist the verified identity and
file hashes. Unchanged same-commit installations reuse verified artifacts without
repeated large downloads or runtime restarts. Safe host drift is reconciled;
disruptive binary/configuration changes with active consumers are refused.

## Failures and boundaries

No kubeadm reset, etcd deletion, reboot, OS provisioning or automatic upgrade.
Conflicting markers, unowned installations and incomplete init/join fail visibly.
A lost control plane with an existing recorded identity must be restored, not
silently reinitialized. Domain identity survives ordinary reboots and named
references are revalidated against resolved UIDs on each pass.

Join credentials are short-lived, private and cleaned up; node admin kubeconfigs
remain root-only. API status contains public observations, never tokens or keys.
SSH loss/timeouts retain operation claims when remote work may continue. Inspect
the worker/node and stop uncertain work before conditionally releasing an exact
Operation. Never clear it merely to make the pipeline run. See
operations-rollout.md for the coordinated API/policy/operator deployment.

Source tests and runner syntax/localhost fixtures are non-mutating. A real cluster
must still pass the acceptance above on the explicitly approved installed nodes.

## Lab acceptance: 2026-10-13 UTC

Configuration build 277 (global build 107115) succeeded with ansible-roles
commit c91fcf59cbeb64c33f36c1dd306bfe271dfa67dc. The recorded verification time
was 02:38:55 UTC. Both installed Debian nodes were Ready with Kubernetes 1.37.1
and containerd 2.4.1; Flannel 0.28.10 and CoreDNS were Running. Temporary pods
passed Kubernetes service DNS on both nodes and HTTP in all four ordered pairs.
The probes were removed, and protected Ready markers on both nodes matched
the Configuration's actual kube-system namespace UID
bc3194f1-58f4-41c8-b841-9f670b77da10. Independent strict-SSH checks confirmed
node/pod state and active containerd, kubelet, homelabd and NetworkManager.
No OS reprovisioning, erasure or kubeadm reset was performed. The temporary
Concourse Git pin was removed after acceptance; normal scheduling continues.
Scheduled build 278 (107202) also succeeded, reused the protected artifact
identities and repeated the networking probes. Beelink had zero configuration
changes; the control-plane changes were the temporary probe lifecycle/result
publication. No runtime service restart or release download was needed.

Inside a normal homelabc shell, access the cluster through its captured,
verified control-plane SSH alias:

```sh
ssh zima 'sudo /usr/local/bin/kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide'
```

Inventory capture membership is not Kubernetes membership. The capture group's
homelab.io/kube-cluster label selects the interactive runner's cluster context;
Server labels select members. The runner verifies the captured control plane's
protected identity and actual cluster namespace before fetching a private,
container-local kubeconfig. Unlabeled groups do not initialize kubectl.
Kubernetes-safe reprovision preparation is also not implemented; RFC 0009 records
the future cleanup-gate design. Do not reprovision cluster members assuming they
will automatically drain.

## Membership reconciliation acceptance: 2026-10-15 UTC

Build 890 (global 158759), ansible-roles commit
72c79d132de1e2a821665c1892d05f212a390615, completed with Configuration Ready.
KubeCluster.status was verified at 22:51:23 UTC, with fresh membership observation,
both Nodes Ready and successful pod DNS and all four ordered HTTP checks.
Beelink's recreated Node received 172.28.2.0/24; the managed CNI configuration
reconciled cni0 from its old gateway to 172.28.2.1 without a cluster reset or
out-of-band bridge cleanup. The ConfigMap checksum rolled both Flannel Pods.
Independent SSH inspection confirmed that gateway and healthy system Pods;
the deployed UI accepted the fresh status as current.

During rollout the platform etcd (not the Kubernetes etcd on Zima) reached its
2 GiB backend quota despite having less than 1 MiB of live data after automatic
compaction. Defragmentation reclaimed the free pages; clearing NOSPACE restored
writes without deleting resources. Compaction alone does not shrink the backend
file. Periodic fragmentation monitoring/defragmentation is still needed for
platform maintenance; raising the quota or resetting resources is not the fix.
