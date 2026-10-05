# Disposable Commands

1. CommandsPipeline owns one persistent Pipeline, Concourse `run` job and stable `commands-<executor-name>` Git branch. Multiple Commands reference that executor.
2. Command is an immutable one-shot request with optional post-completion TTL. Acceptance snapshots runner settings; publication writes its UID-owned script directory and records the exact commit.
3. Claim `CommandsPipeline.status.activeCommandRef` with a compare-and-swap. Only that Command may configure and submit to the shared job; others wait. Hold the slot until terminal observation, including queued builds and uncertain submission responses.
4. Install the accepted job configuration with the exact script path and commit. Register the Git version using `fly check-resource`. Persist Dispatching and the latest prior build ID before the JSON job-build POST; reuse authentication issued by standard `fly login`.
5. Track the returned build ID directly. On response loss, adopt only a sole build newer than the recorded baseline. Missing or ambiguous results reserve the slot without resubmission. Operators must investigate uncertainty before clearing dispatch state; exactly-once execution is not claimed.
6. Command deletion/TTL aborts and observes only its build, removes only its Git directory and releases its slot. Never delete the shared pipeline or another request's inputs. Executor deletion waits for its Commands and then deletes its owned Pipeline.
7. Optional scheduling creates fresh Commands, skips overlapping scheduled runs and missed intervals, and persists its scheduling cursor before creation. No Git/time execution triggers or automatic build retries.

Concourse's job-build endpoint does not accept per-build input overrides. Serializing the complete execution protects queued builds from shared job configuration changes. Distinct executors can run independently, including executors referencing the same capture group. Controller-managed jobs must not be triggered or edited manually.
