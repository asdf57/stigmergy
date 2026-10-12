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

Read the target Server and capture/repository resource UIDs first. Require a
verified successful installed OS, current reciprocal Machine binding and Ready
management SSH. Configuration does not install software in the live ISO.

For the supplied containerd playbook:

```yaml
apiVersion: homelab.io/v1alpha1
kind: Configuration
metadata:
  name: node-runtime
spec:
  targetRef: {kind: Server, name: node, uid: <server-uid>}
  inventoryCaptureGroupRef: {name: ssh-managed, uid: <group-uid>}
  playbook:
    repositoryRef: {name: ansible-roles, uid: <repository-uid>}
    revision: <full-commit-containing-the-playbook>
    path: plays/configure_containerd.yml
  values:
    packageReference: <exact-distro-package-reference>
    version: <exact-upstream-runtime-version>
```

Review the exact package's availability before applying this object. The existing
containerd role refuses disruptive reconfiguration with active Docker/kubelet
consumers; this adapter does not bypass that maintenance safeguard.

For a Kubernetes platform, create KubeCluster settings, label its approved Servers
with homelab.io/kube-cluster and homelab.io/kube-role, and bind
homelab.io/kube-cluster-uid in annotations. Then target that cluster with one
Configuration and an ordered, verified platform playbook. The executor supplies
control_plane/worker groups, configuration_target settings and
configuration_nodes. kubeadm bootstrap/join/platform roles are not supplied by
the generic executor; implement and accept those before creating the setup.

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
