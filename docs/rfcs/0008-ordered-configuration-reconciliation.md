# RFC 0008: Ordered configuration reconciliation

Status: Generic argv execution implemented in source; coordinated rollout pending.

## Contract

Configuration represents one coherent, maintained setup, not a disposable job.
It declares a source repository and command.argv. The executor runs the argument
array directly in the resolved checkout: no implicit shell, variable expansion,
argument rewriting or top-level injection of Configuration.values. An explicitly
declared shell is ordinary executable code and has the same trusted-operator
authority as any other command.

dependsOn orders Configurations; commands/playbooks define their internal order.
ProvisioningRun alone authorizes OS replacement. Configuration never authorizes
disk erasure, cluster reset, disruptive upgrades or uncontrolled recovery.

```yaml
apiVersion: homelab.io/v1alpha1
kind: Configuration
metadata:
  name: lab-grafana
spec:
  targetRef: {kind: KubeCluster, name: lab}
  inventoryCaptureGroupRef: {name: ssh-managed}
  source:
    repositoryRef: {name: ansible-roles}
  command:
    argv: [ansible-playbook, plays/grafana.yml, -e, mode=install]
  context:
    kubeClusterRef: {name: lab}
  dependsOn:
    - name: kube-setup-lab
```

source.revision optionally pins a full commit. Otherwise GitRepository.spec.ref
resolves from the pipeline Git input. The input origin must match the repository;
missing refs never cause remote fetching or fallback. Safe archive extraction
rejects escaping paths/links. Commands run inside that private checkout. The
resolved commit is recorded as Configuration.status.sourceRevision.

targetRef supplies public context, not automatic host selection or readiness.
The complete Server-only capture group is the execution boundary: omitted
resources defer execution rather than silently shrinking it. Every captured
Server must have current reciprocal Machine binding, a successful installation
for its UID, verified SSH identity and no active provisioning. One Operation is
acquired per Server in deterministic order, including when a command only uses
Kubernetes. API tokens retain their existing scope; no new Secret reads.

## Execution environment

Each pass owns a private temporary directory containing code/, inventory.json,
values.json, diagnostics and optional kubeconfig/bin. The command working directory
is code/. ANSIBLE_INVENTORY, ANSIBLE_ROLES_PATH and ANSIBLE_FILTER_PLUGINS point
at the prepared inventory and extracted roles/plugins. Implicit localhost remains
local/unprivileged; managed hosts have explicit verified SSH/become host variables.

CONFIGURATION_CONTEXT_FILE identifies private values.json. It contains public
configuration_values, configuration_target, configuration_nodes, configuration_ref,
configuration_generation, configuration_input_revision, configuration_source_revision,
configuration_observed and configuration_workspace. CONFIGURATION_WORKSPACE
also exposes the workspace path to non-Ansible commands.

Commands explicitly opt into these inputs. For example Kubernetes setup uses
argv: [ansible-playbook, plays/configure_kube.yml, -e, "@../values.json"].
That is a literal relative path from code/, not a template or shell expansion.
Helm plays instead use ordinary -e mode=install. Do not place credentials in
argv, Configuration.values, result files or public status.

The installed-host Ansible guard runs before and after the command. Ownership
is checked before each stage and inputs are revalidated before publishing success.
Command output streams into Concourse and a private log. Timeouts kill only the
local process group and retain Uncertain ownership because remote work may remain.
SSH transport failure and signal interruption also retain ownership. Known
completed failures report Failed; no automatic retry or expiry releases claims.
The reviewed command must verify its own component health; exit zero is not
independent proof of arbitrary application health.

## Explicit Kubernetes context

context.kubeClusterRef opts into a verified existing cluster context. Bootstrap
omits it, so a cluster does not need to exist before its setup command runs.
The referenced cluster lifetime/settings participate in inputRevision and
resolvedReferences independently of targetRef.

Shared runner_kubernetes code selects exactly one labeled, captured control
plane, checks the protected installed Server/cluster marker, settings, actual
Kubernetes namespace and identity ConfigMap, and retrieves admin.conf over strict
UID-qualified SSH. Only a self-contained TLS client-certificate context for the
desired endpoint is accepted; no exec plugins, file references or TLS bypass.
Server/cluster lifetimes and inputs are checked again after retrieval.

KUBECONFIG points to the workspace's mode-0600 file. A checksum-verified kubectl
matching the cluster version is installed in its private bin directory. Contexts
are never written to shared homes or API status, and disappear with the execution
workspace. No context is inherited from another Configuration. Failed retrieval
stops execution. Admin context is the explicit v1 learning policy, not fine-grained
Kubernetes RBAC; it requires no API Secret grant.

Interactive homelabc keeps its existing context association through the capture
group label homelab.io/kube-cluster and writes private ~/.kube/config. That label
does not enroll nodes. Ordinary Command startup still does not fetch admin
context; Configuration preparation is explicit and scoped to the declared ref.

## Dependencies, observations and drift

Each scheduled/manual pass traverses dependencies first. Dependencies must be
Ready for their current generation, resolved references and inputRevision; a
historical green result does not satisfy changed source, installation or identity.
Cycles fail visibly. Heartbeats and transient Operation references do not change
inputRevision; command/source/context/spec/member changes do.

Eligible commands rerun idempotently each pass, even without spec changes, to
inspect and repair safe drift. One shared Concourse operator pipeline handles the
capture group; there are no per-role pipelines or timer Command objects.
suspended stops reconciliation without uninstalling. Deletion leaves installed
software intact. Uninstall is an explicit command change with mode=uninstall.

Commands may write bounded public result.json, target-status.json and
target-observation.json in the workspace. Symlinks, special files and files over
64 KiB are refused. Success output is published only after guards, ownership and
input revalidation. Completed failures may publish observation-only output, never
success or observations after uncertain remote work. Domain commands own their
status schema; generic execution does not synthesize Kubernetes health.

Kubernetes setup remains one ordered configure_kube.yml command for containerd,
kubeadm, membership and Flannel. Its observation and verified snapshot remain on
KubeCluster.status, with domain playbookRevision compared to the reporting
Configuration's sourceRevision. Desired members are selected by Server labels;
initialized nodes authenticate existing kubelet identity and reconcile actual Node
registration/labels without reset or blind join replay. Node/workload verification
covers ownership, versions, managed addresses, pod subnets, DNS and cross-node HTTP.
Historical snapshots and current negative/stale membership observations remain
distinct. The UI reads generic API status, never Kubernetes credentials.

## Implementation and rollout

- stigmergy: Configuration source/command/context schemas, generated API/docs.
- ansible-roles/operators/configurations.py: dependency/input resolution, checkout,
  guarded direct execution, explicit context preparation, Operation/status reporting.
- operators/runner_kubernetes.py: shared verified context retrieval.
- homelab-init/Configuration: kube-setup-lab, lab-node-exporter and lab-grafana.
  Both Helm installations depend on kube-setup-lab and independently request lab's context.
- stigmergy-web: generic source/context references and revision observations.

Drain/pause configuration execution before deploying the coordinated schema and
executor. Preserve existing tokens, Git keys, desired resources and node state.
Update existing Configuration specs through conditional API writes to the current
contract, then enable the two reviewed application resources. Do not run init or
provision nodes as a substitute for a scoped rollout. Verify real Operations,
Configuration source revisions, Helm readiness and actual cluster state afterward.
Source tests do not constitute deployment acceptance.
