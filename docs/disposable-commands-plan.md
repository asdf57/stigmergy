# Disposable Commands

Implementation plan:

1. Make Command an immutable execution request: executor reference, script, optional post-completion TTL. Keep CommandsPipeline as reusable settings, optionally an interval plus command template.
2. Snapshot accepted settings. Publish each script into a UID-owned Git directory; pin the resulting commit. Create a UID-owned Pipeline through the existing Pipeline controller, without automatic Git/time triggers. Explicitly register the pinned version with native fly check-resource before submission; a newly configured resource otherwise initially discovers only the latest commit.
3. Persist Dispatching before submitting one Concourse build. Track its ID and terminal outcome. Recover a lost submission response by reading that isolated job's builds, never resubmitting automatically.
4. Poll execution status; implement terminal TTL and finalizer cleanup (abort active builds, delete owned pipeline, then remove owned Git inputs). Never delete unowned resources.
5. Have the reusable executor create deterministic, interval-named Commands. Skip missed intervals and overlapping scheduled runs; claim each scheduling slot before creation and retain the cursor across restart and TTL deletion (a crash between claim and create may skip a slot).
6. Regenerate API models/docs, update bootstrap manifests and the RFC, and test lifecycle, concurrent requests, immutability, uncertain dispatch, scheduling and cleanup.

Tradeoff: one Concourse pipeline per retained Command costs more objects, but uses the existing generic Pipeline lifecycle and avoids shared-input races. TTL bounds retained execution state. Commands do not retry automatically; a new request is the explicit retry. Dispatching can remain uncertain if the controller crashed before submitting: exactly-once execution cannot be guaranteed across an external API without an idempotency key.

Disposable Command manifests are execution requests, not continuously reapplied desired configuration.
