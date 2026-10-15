# Observable Operation rollout

Operation spec is immutable: action, operator, UID-qualified targetRef and optional
requestedBy. POST acquires one target atomically. Its API-owned operationRef is
part of the generated status contract for every status-bearing resource.
The standard API exposes /api/v1alpha1/operations and /operations/{name}/status.
Ordinary status patches cannot acquire, replace or clear operationRef.

Running work reports currentStep/message. Succeeded and Failed atomically clear
the exact target reference. Uncertain retains it until remote work is inspected
and an explicit terminal outcome is reported. Active records and their targets
cannot be deleted. There is no lease expiry, automatic retry or TTL cleanup.
ProvisioningRun retains its existing full-installation reservation and excludes
Operations; this feature does not authorize a provisioning run or reboot.

## Coordinated deployment

1. Pause the configuration, SSH and Commands jobs and let in-flight executions
   finish. Inspect retained/uncertain remote work; never clear it to make rollout
   pass. Do not change ownership contracts underneath a running executor.
2. Prepare the existing API policy with create-api-auth --policy-source into a
   fresh private directory. Runner gains Operation GET/POST and status PATCH;
   it does not gain DELETE, target-spec writes or Secret reads. Preserve tokens.
   Update the private bootstrap policy too. Install runtime policy as numeric
   UID/GID 65532:65532 mode 0400; replacing its bind-mounted inode requires
   recreating the API container. Owner-required one-time privileged installation
   is manual, not become/recurring ownership repair in initialization.
3. Deploy the generated API and verify OpenAPI Operation routes/common operationRef,
   runner authorization and API readiness before publishing the new executors.
4. Deploy the matching operator code and web UI, then resume jobs. The API schema
   and clients are one version; do not run mixed claim contracts or add fallback
   implementations. Preserve desired resources, credentials and node state.
5. Observe real configuration/SSH Operations, their target/requester references,
   public progress and terminal release. Verify existing cluster readiness and
   KubeCluster observations; never test UI confirmation by provisioning/rebooting.

## Verification

Run go test ./..., isolated etcd integration tests, ansible-roles make check,
npm test and npm run build. Tests cover two-target revision fences, stale/replaced
identities, single ownership, provisioning exclusion, API-owned references,
uncertainty retention, authorization and read-only UI polling.

The browser exposes Operations under Automation and on every target detail page.
Operation details poll public status every five seconds. Provision/reboot retain
explicit warning/action dialogs but no name typing. Provisioning refreshes its
Server snapshot before loading hardware; conflicts retain the actual API reason.
