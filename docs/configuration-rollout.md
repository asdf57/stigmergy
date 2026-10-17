# Configuration reconciliation

## Verified deployment: 2026-10-17 UTC

Build 1007 (global ID 168982) verified all three site Configurations using source
878bf9f: kube-setup-lab at generation 3, lab-grafana and lab-node-exporter at
generation 1. All six Operations completed successfully and released ownership.
Exporter is Ready on both nodes and serves metrics; Grafana is Ready and its
database health endpoint reports ok. Cluster DNS and cross-node HTTP passed.
The API/UI are deployed and Configuration/SSH schedules are unpaused.

Before cutover, the owner reclaimed compacted etcd space and cleared its NOSPACE
alarm; writes and normal migration then succeeded. No automatic repair job,
quota workaround, token rotation, reboot or reprovision was added.

## Source and command execution

`spec.source.repositoryRef` selects the trusted pipeline Git input; optional
`source.revision` pins a full commit. `spec.command.argv` is executed directly
from its extracted checkout, without shell or template expansion. Arguments use
ordinary tool syntax, for example `[ansible-playbook, plays/grafana.yml, -e,
mode=install]`. Public values/context are in `CONFIGURATION_CONTEXT_FILE`;
Ansible plays that need them explicitly pass `-e @../values.json`. Inventory and
role/plugin paths are already configured in the command environment.

`context.kubeClusterRef: {name: lab}` explicitly requests a private Kubernetes
context, verified through a captured control plane's managed SSH and protected
cluster identity. Bootstrap omits it. Retrieval failure prevents execution, and
no previous context is inherited. The workspace owns mode-0600 kubeconfig and
matching checksum-verified kubectl; credentials never enter API status/values.
No API token or Secret-read permissions are changed.

Site manifests are `Configuration/configuration-kube-lab.yaml`,
`Configuration/configuration-node-exporter-lab.yaml` and
`Configuration/configuration-grafana-lab.yaml` in homelab-init. Both application
setups depend on current `kube-setup-lab` verification, not on each other.

For rollout, pause/drain configuration execution, deploy matching API and
executor revisions, and conditionally update the existing setup spec to this
contract before applying the two new Configurations. Preserve tokens, identities
and node state. Then resume and verify actual Helm releases and Operations;
source tests alone are not deployment acceptance. Do not upload the whole site
or reprovision nodes just to apply these three resources.

Refresh the shared pipeline's runner image input to the published Helm-capable
image before resuming (`fly -t homelab check-resource -r
reconcile-configurations-ssh-managed/runner`). No new per-Configuration pipeline
or API permission grant is needed.

Configuration maintains a coherent setup on installed Servers independently of
ProvisioningRuns. Its repository-backed command defines role order. dependsOn links separate
setups. The full contract is RFC 0008.

## Stand up the executor

1. Deploy the API containing Configuration and KubeCluster schemas/generated
   routes. No API controller executes these commands.
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
  source:
    repositoryRef: {name: ansible-roles}
  command:
    argv: [ansible-playbook, plays/configure_containerd.yml, -e, "@../values.json"]
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
not cause a fallback or remote fetch. Status records the actual sourceRevision
and resolvedReferences including UIDs/generations. Changed code or recreated
references invalidate old verification.

targetRef supplies settings/context, not automatic readiness or host selection.
The executor preserves the Server-only capture inventory and validates/locks
every captured installed host. The playbook selects hosts/groups within that
boundary. A Server target does not implicitly limit a broad capture to that Server;
use an appropriately scoped capture group or reviewed playbook selection.
dependsOn supplies explicit readiness dependencies.

For a Kubernetes platform, create KubeCluster settings, label its approved Servers
with homelab.io/kube-cluster and homelab.io/kube-role. Optionally bind
homelab.io/kube-cluster-uid in annotations for a stricter lifetime constraint.
Then target that cluster with one
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
Local runner plays must set `vars: {ansible_become: false}` as well as
`become: false`; Ansible connection variables outrank play keywords, including
captured `all.vars`. Apply the same task-variable override to delegated local
result writes. Keep node escalation scoped to managed-node tasks.

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
