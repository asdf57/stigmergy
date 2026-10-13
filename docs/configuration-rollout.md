# Configuration reconciliation

Configuration maintains a coherent setup on installed Servers independently of
ProvisioningRuns. Its pinned playbook defines role order. dependsOn links separate
setups. The full contract is RFC 0008.

## Stand up the executor

1. Deploy the API containing Configuration and KubeCluster schemas/generated
   routes. No API controller executes these playbooks.
2. Prepare the runner policy using the existing policy and a fresh private
   output directory:

   ```sh
   go run ./cmd/create-api-auth --policy-source /private/existing/api-access.json --output-dir /private/new-configuration-policy
   ```

   This preserves all tokens. It adds public Configuration/KubeCluster/
   GitRepository reads and Configuration status PATCH, not Secret reads or
   provisioning-request creation. Install the policy through the established
   token-preserving bootstrap workflow; never print it.
3. Publish the reviewed ansible-roles executor and playbooks. Apply only
   homelab-init/GitRepository/git-repository-ansible-roles.yaml and
   homelab-init/Pipeline/pipeline-configurations.yaml through the existing admin
   workflow. Preserve unrelated resources and credentials. This creates no
   executable Configuration or cluster membership.
4. Verify the empty executor pass is an idle no-op. The shared job runs every
   five minutes and can be triggered manually. Do not run full init merely to
   install this pipeline.

## Declare an explicitly approved setup

Use unique names for target, capture group, repository and dependency references. Require a
verified successful installed OS, current reciprocal Machine binding and Ready
management SSH. Configuration does not install software in the live ISO.

For the supplied containerd playbook:

```yaml
apiVersion: homelab.io/v1alpha1
kind: Configuration
metadata:
  name: node-runtime
spec:
  targetRef: {kind: Server, name: node}
  inventoryCaptureGroupRef: {name: ssh-managed}
  playbook:
    repositoryRef: {name: ansible-roles}
    path: plays/configure_containerd.yml
  values:
    packageReference: <exact-distro-package-reference>
    version: <exact-upstream-runtime-version>
```

Review the exact package's availability before applying this object. The existing
containerd role refuses disruptive reconfiguration with active Docker/kubelet
consumers; this adapter does not bypass that maintenance safeguard.

An omitted revision resolves GitRepository.spec.ref (default main) from the
pipeline's local Git input. An optional revision pins a full 40-character commit.
The pipeline input must contain the selected ref/commit; missing history does
not cause a fallback or remote fetch. Status records the actual playbookRevision
and resolvedReferences including UIDs/generations. Changed code or recreated
references invalidate old verification.

targetRef supplies settings/context, not automatic readiness or host selection.
The executor preserves the Server-only capture inventory and validates/locks
every captured installed host. The playbook selects hosts/groups within that
boundary. A Server target does not implicitly limit a broad capture to that Server;
use an appropriately scoped capture group or reviewed playbook selection.
dependsOn supplies explicit readiness dependencies.

For a Kubernetes platform, create KubeCluster settings, label its approved Servers
with homelab.io/kube-cluster and homelab.io/kube-role, and bind
homelab.io/kube-cluster-uid in annotations. Then target that cluster with one
Configuration and an ordered, verified platform playbook. The executor supplies
configuration_target settings and configuration_nodes with captured Server labels
and annotations. The platform playbook validates membership and creates its
control_plane/worker groups. plays/configure_kube.yml supplies the ordered
kubeadm/Flannel platform through kube and containerd roles; the executor itself
contains no Kubernetes bootstrap logic. Follow kubernetes-rollout.md for acceptance.

Kubernetes planning/preflight is available separately in plays/inspect_kube.yml.
It selects labeled cluster members, validates inputs and connected networks,
checks endpoint DNS and reports existing installation artifacts without changing
nodes. Its success means inspection passed, not a Ready Kubernetes platform.
Do not replace the site's configure_kube.yml path with this inspection playbook
to make the platform Configuration appear successful.

Reviewed playbooks may write public observations to
configuration_workspace/result.json (a regular JSON object, at most 64 KiB).
The executor records these as status.outputs only after verification; previous
outputs are supplied as configuration_observed. Never publish credentials.
Kubernetes uses this for its verified installed namespace identity and versions.

Use configuration_values for public role parameters. Do not interpolate them as
shell, put credentials in them or override protected inventory/transport inputs.
Fetch credentials through reviewed no_log tasks and the private runner context.
Component playbooks must verify actual services/API objects before succeeding.

## Reconcile and inspect

```sh
fly -t homelab trigger-job -j reconcile-configurations-ssh-managed/operator
fly -t homelab watch -j reconcile-configurations-ssh-managed/operator
```

Read /api/v1alpha1/configurations/<name> for phase, reason, observedGeneration,
inputRevision and lastVerifiedAt. Normal Ansible tasks stream to the build.
Dependencies must match current desired generation and resolved input revision;
old successes after reprovisioning never satisfy them. A successful build alone
does not mean every setup is Ready.

suspended: true stops future passes, not remote work already in progress or
software already installed. Deleting the object does not uninstall anything.
Missing/replaced dependencies, membership conflicts and unsafe source revisions
fail closed. Never force a Ready status to unblock dependents.

An uncertain timeout or killed worker can retain Server operation claims. Inspect
the build, worker and remote tasks before conditionally releasing that exact
stopped claim. No automatic claim expiry or erasure retry exists. No cluster
reset, etcd deletion, upgrade or disk provisioning is authorized by applying
the executor infrastructure.
