package command

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/controller/commandspipeline"
	"github.com/asdf57/stigmergy/internal/controller/pipeline"
	"github.com/asdf57/stigmergy/internal/gitpublication"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const cleanupFinalizer = "homelab.io/command-cleanup"
const ownerAnnotation = "homelab.io/command-uid"
const revisionPlaceholder = "__STIGMERGY_COMMAND_REVISION__"

type Reconciler struct {
	store     store.Store
	publisher gitpublication.Publisher
	executor  *commandspipeline.Reconciler
	backend   pipeline.ExecutionBackend
	now       func() time.Time
}

func NewReconciler(s store.Store, config commandspipeline.Config) *Reconciler {
	return NewReconcilerWithDependencies(s, config, gitpublication.NewGitPublisher(s), pipeline.NewFlyBackend())
}
func NewReconcilerWithDependencies(s store.Store, config commandspipeline.Config, publisher gitpublication.Publisher, backend pipeline.ExecutionBackend) *Reconciler {
	return &Reconciler{store: s, publisher: publisher, executor: commandspipeline.NewReconciler(s, config), backend: backend, now: time.Now}
}
func executionName(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("command-%x", sum[:16])
}
func terminal(v registry.Command) bool {
	return v.Status != nil && v.Status.Phase != nil && (*v.Status.Phase == apigen.CommandStatusPhaseSucceeded || *v.Status.Phase == apigen.CommandStatusPhaseFailed)
}
func version(v resource.Metadata) (int64, error) { return strconv.ParseInt(v.ResourceVersion, 10, 64) }

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.CommandResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	v, err := registry.CommandResource.Decode(raw)
	if err != nil {
		return err
	}
	if v.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, v)
	}
	if !slices.Contains(v.Metadata.Finalizers, cleanupFinalizer) {
		v.Metadata.Finalizers = append(v.Metadata.Finalizers, cleanupFinalizer)
		encoded, err := v.Encode()
		if err != nil {
			return err
		}
		rv, err := version(v.Metadata)
		if err != nil {
			return err
		}
		_, err = r.store.Update(ctx, encoded, rv)
		return err
	}
	if terminal(v) {
		if v.Spec.TtlSecondsAfterFinished != nil && v.Status.CompletedAt != nil && !r.now().Before(v.Status.CompletedAt.Add(time.Duration(*v.Spec.TtlSecondsAfterFinished)*time.Second)) {
			rv, err := version(v.Metadata)
			if err != nil {
				return err
			}
			return r.store.Delete(ctx, v.Kind, v.Metadata.Name, rv)
		}
		return nil
	}
	if v.Status == nil {
		v.Status = &apigen.CommandStatus{}
	}
	if v.Status.Snapshot == nil {
		return r.accept(ctx, v)
	}
	name := executionName(v.Metadata.UID)
	// Dispatch state is durable. Never recreate/re-submit an execution after this boundary.
	if v.Status.BuildID != nil || (v.Status.Phase != nil && *v.Status.Phase == apigen.CommandStatusPhaseDispatching) {
		return r.observe(ctx, v, name)
	}
	if v.Status.Revision == nil {
		result, err := r.publisher.Publish(ctx, gitpublication.PublishRequest{Repository: v.Status.Snapshot.Repository, PublicationName: v.Metadata.Name, Branch: v.Status.Snapshot.Branch, RootPath: name, OwnerUID: v.Metadata.UID, PreserveUnmanaged: true, Artifacts: []gitpublication.Artifact{{Path: "run.sh", Content: []byte(v.Spec.Script)}}})
		if err != nil {
			return fmt.Errorf("publish Command inputs: %w", err)
		}
		if result.Revision == "" {
			return fmt.Errorf("publication returned no Git revision")
		}
		v.Status.Revision = &result.Revision
		return r.save(ctx, v, apigen.CommandStatusPhasePending, "InputsPublished", "Immutable script inputs published")
	}
	child, err := r.ensurePipeline(ctx, v, name)
	if err != nil {
		return err
	}
	v.Status.PipelineRef = &apigen.ResourceReference{Name: child.Metadata.Name, Uid: child.Metadata.UID}
	if child.Status != nil && child.Status.Phase != nil && *child.Status.Phase == apigen.PipelineStatusPhaseFailed && child.Status.ObservedGeneration != nil && *child.Status.ObservedGeneration == child.Metadata.Generation {
		return r.save(ctx, v, apigen.CommandStatusPhaseFailed, "PipelineFailed", "Execution pipeline configuration failed; create a new Command after fixing the executor")
	}
	if child.Status == nil || child.Status.Phase == nil || *child.Status.Phase != apigen.PipelineStatusPhaseReady || child.Status.ObservedGeneration == nil || *child.Status.ObservedGeneration != child.Metadata.Generation {
		return r.save(ctx, v, apigen.CommandStatusPhasePending, "PipelinePending", "Waiting for the execution pipeline")
	}
	provider, credential, err := r.connection(ctx, v)
	if err != nil {
		return err
	}
	if err := r.backend.CheckResource(ctx, provider, credential, name, "commands", map[string]string{"ref": *v.Status.Revision}); err != nil {
		return fmt.Errorf("register immutable command input version: %w", err)
	}
	// A CAS wins the right to submit. A crash after this write is recovered by observation, not retry.
	updated, err := r.persist(ctx, v, apigen.CommandStatusPhaseDispatching, "Submitting", "Submitting one Concourse build")
	if err != nil {
		return err
	}
	build, err := r.backend.Trigger(ctx, provider, credential, name)
	if err != nil {
		return r.save(ctx, updated, apigen.CommandStatusPhaseDispatching, "SubmissionUncertain", "Submission result is unknown; observing builds without resubmitting")
	}
	updated.Status.BuildID = &build.ID
	return r.saveBuild(ctx, updated, build)
}
func (r *Reconciler) accept(ctx context.Context, v registry.Command) error {
	raw, err := r.store.Get(ctx, registry.CommandsPipelineResource.Kind, v.Spec.CommandsPipelineRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.save(ctx, v, apigen.CommandStatusPhasePending, "ExecutorNotFound", "Waiting for the referenced CommandsPipeline")
	}
	if err != nil {
		return err
	}
	executor, err := registry.CommandsPipelineResource.Decode(raw)
	if err != nil {
		return err
	}
	// Recheck migration independently of the executor's cached Ready status;
	// the two controllers may restart in either order.
	legacy, legacyErr := r.store.Get(ctx, registry.PipelineResource.Kind, executor.Metadata.Name)
	if legacyErr == nil && legacy.Metadata.Annotations["homelab.io/commands-pipeline-uid"] == executor.Metadata.UID {
		return r.save(ctx, v, apigen.CommandStatusPhasePending, "LegacyPipelinePresent", "Retire the legacy command Pipeline before accepting new executions")
	}
	if legacyErr != nil && !errors.Is(legacyErr, store.ErrNotFound) {
		return legacyErr
	}
	if v.Spec.CommandsPipelineRef.Uid != nil && *v.Spec.CommandsPipelineRef.Uid != executor.Metadata.UID {
		return r.save(ctx, v, apigen.CommandStatusPhaseFailed, "ExecutorReplaced", "The referenced executor UID no longer matches")
	}
	if executor.Metadata.DeletionTimestamp != nil || executor.Status == nil || executor.Status.Phase == nil || *executor.Status.Phase != apigen.CommandsPipelineStatusPhaseReady || executor.Status.ObservedGeneration == nil || *executor.Status.ObservedGeneration != executor.Metadata.Generation {
		return r.save(ctx, v, apigen.CommandStatusPhasePending, "ExecutorNotReady", "Waiting for the referenced CommandsPipeline to become Ready")
	}
	name := executionName(v.Metadata.UID)
	branch := executionName(executor.Metadata.UID)
	repository, provider, definition, err := r.executor.Prepare(ctx, executor, branch, name+"/run.sh", revisionPlaceholder)
	if err != nil {
		return err
	}
	v.Status.Snapshot = &apigen.CommandExecutionSnapshot{ExecutorUID: executor.Metadata.UID, Repository: repository.Spec, Provider: provider.Spec, ProviderUID: provider.Metadata.UID, ProviderName: provider.Metadata.Name, Branch: branch, Definition: definition}
	return r.save(ctx, v, apigen.CommandStatusPhasePending, "Accepted", "Runner settings snapshotted for this execution")
}
func (r *Reconciler) ensurePipeline(ctx context.Context, v registry.Command, name string) (registry.Pipeline, error) {
	data := strings.ReplaceAll(v.Status.Snapshot.Definition, revisionPlaceholder, *v.Status.Revision)
	desired := registry.NewPipeline(resource.Metadata{Name: name, Finalizers: append([]string(nil), registry.PipelineResource.DefaultFinalizers...), Annotations: map[string]string{ownerAnnotation: v.Metadata.UID}}, apigen.PipelineSpec{ProviderRef: apigen.PipelineProviderReference{Name: v.Status.Snapshot.ProviderName}, ExternalName: name, Definition: apigen.PipelineDefinition{Format: apigen.PipelineDefinitionFormatConcourse, Data: data}})
	raw, err := r.store.Get(ctx, desired.Kind, name)
	if errors.Is(err, store.ErrNotFound) {
		encoded, encodeErr := desired.Encode()
		if encodeErr != nil {
			return registry.Pipeline{}, encodeErr
		}
		raw, err = r.store.Create(ctx, encoded)
		if errors.Is(err, store.ErrConflict) {
			return r.ensurePipeline(ctx, v, name)
		}
	}
	if err != nil {
		return registry.Pipeline{}, err
	}
	child, err := registry.PipelineResource.Decode(raw)
	if err != nil {
		return child, err
	}
	if child.Metadata.Annotations[ownerAnnotation] != v.Metadata.UID || !resource.EqualJSON(child.Spec, desired.Spec) || child.Metadata.DeletionTimestamp != nil {
		return child, fmt.Errorf("execution Pipeline ownership/spec conflict")
	}
	return child, nil
}
func (r *Reconciler) connection(ctx context.Context, v registry.Command) (registry.PipelineProvider, registry.UsernamePasswordCredential, error) {
	snap := v.Status.Snapshot
	raw, err := r.store.Get(ctx, registry.PipelineProviderResource.Kind, snap.ProviderName)
	if err != nil {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, err
	}
	provider, err := registry.PipelineProviderResource.Decode(raw)
	if err != nil {
		return provider, registry.UsernamePasswordCredential{}, err
	}
	if provider.Metadata.UID != snap.ProviderUID || provider.Spec.Type != snap.Provider.Type || provider.Spec.Url != snap.Provider.Url || provider.Spec.Team != snap.Provider.Team {
		return provider, registry.UsernamePasswordCredential{}, fmt.Errorf("execution provider identity/endpoint changed; restore it before observing or cleaning up")
	}
	raw, err = r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, provider.Spec.CredentialRef.Name)
	if err != nil {
		return provider, registry.UsernamePasswordCredential{}, err
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(raw)
	return provider, credential, err
}
func (r *Reconciler) observe(ctx context.Context, v registry.Command, name string) error {
	provider, credential, err := r.connection(ctx, v)
	if err != nil {
		return err
	}
	if v.Status.BuildID == nil {
		builds, err := r.backend.Builds(ctx, provider, credential, name)
		if err != nil {
			return err
		}
		if len(builds) == 0 {
			return r.save(ctx, v, apigen.CommandStatusPhaseDispatching, "SubmissionUncertain", "No build found yet; never automatically resubmitting. Delete this request and create a new one if an explicit retry is required.")
		}
		if len(builds) != 1 || builds[0].ID < 1 {
			return fmt.Errorf("isolated execution job has unexpected build history; refusing ambiguous adoption")
		}
		v.Status.BuildID = &builds[0].ID
		return r.saveBuild(ctx, v, builds[0])
	}
	build, err := r.backend.Build(ctx, provider, credential, name, *v.Status.BuildID)
	if err != nil {
		return err
	}
	return r.saveBuild(ctx, v, build)
}
func (r *Reconciler) saveBuild(ctx context.Context, v registry.Command, build pipeline.Build) error {
	phase := apigen.CommandStatusPhaseRunning
	if build.Terminal() {
		phase = apigen.CommandStatusPhaseFailed
		if build.Status == "succeeded" {
			phase = apigen.CommandStatusPhaseSucceeded
		}
		if v.Status.CompletedAt == nil {
			now := r.now().UTC()
			v.Status.CompletedAt = &now
		}
	}
	return r.save(ctx, v, phase, "BuildObserved", "Concourse build is "+build.Status)
}
func (r *Reconciler) save(ctx context.Context, v registry.Command, phase apigen.CommandStatusPhase, reason, message string) error {
	_, err := r.persist(ctx, v, phase, reason, message)
	return err
}
func (r *Reconciler) persist(ctx context.Context, v registry.Command, phase apigen.CommandStatusPhase, reason, message string) (registry.Command, error) {
	if v.Status == nil {
		v.Status = &apigen.CommandStatus{}
	}
	generation := v.Metadata.Generation
	v.Status.Phase = &phase
	v.Status.ObservedGeneration = &generation
	condition := apigen.CommandConditionStatusFalse
	if phase == apigen.CommandStatusPhaseSucceeded {
		condition = apigen.CommandConditionStatusTrue
	}
	if phase == apigen.CommandStatusPhaseDispatching {
		condition = apigen.CommandConditionStatusUnknown
	}
	conditions := []apigen.CommandCondition{{Type: "Complete", Status: condition, Reason: reason, Message: &message, ObservedGeneration: &generation}}
	v.Status.Conditions = &conditions
	if (phase == apigen.CommandStatusPhaseSucceeded || phase == apigen.CommandStatusPhaseFailed) && v.Status.CompletedAt == nil {
		now := r.now().UTC()
		v.Status.CompletedAt = &now
	}
	encoded, err := registry.CommandResource.EncodeStatus(v.Status)
	if err != nil {
		return v, err
	}
	raw, err := r.store.Get(ctx, v.Kind, v.Metadata.Name)
	if err != nil {
		return v, err
	}
	if raw.Metadata.ResourceVersion != v.Metadata.ResourceVersion || raw.Metadata.DeletionTimestamp != nil {
		return v, store.ErrConflict
	}
	if resource.EqualJSON(raw.Status, encoded) {
		return registry.CommandResource.Decode(raw)
	}
	rv, err := version(v.Metadata)
	if err != nil {
		return v, err
	}
	updated, err := r.store.UpdateStatus(ctx, v.Kind, v.Metadata.Name, encoded, rv)
	if err != nil {
		return v, err
	}
	return registry.CommandResource.Decode(updated)
}
func (r *Reconciler) finalize(ctx context.Context, v registry.Command) error {
	if !slices.Contains(v.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	name := executionName(v.Metadata.UID)
	if v.Status != nil && v.Status.Snapshot != nil {
		raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		childExists := err == nil
		if childExists && raw.Metadata.Annotations[ownerAnnotation] != v.Metadata.UID {
			return fmt.Errorf("refusing to delete unowned Pipeline")
		}
		// Once the child finalizer completes, Concourse may also have removed its build history.
		// Observe/abort before requesting child deletion, not after its destruction.
		if childExists && (v.Status.BuildID != nil || (v.Status.Phase != nil && *v.Status.Phase == apigen.CommandStatusPhaseDispatching)) {
			provider, credential, err := r.connection(ctx, v)
			if err != nil {
				return err
			}
			var builds []pipeline.Build
			if v.Status.BuildID != nil {
				build, err := r.backend.Build(ctx, provider, credential, name, *v.Status.BuildID)
				if err != nil {
					return err
				}
				builds = []pipeline.Build{build}
			} else {
				var err error
				builds, err = r.backend.Builds(ctx, provider, credential, name)
				if err != nil {
					return err
				}
			}
			waiting := false
			for _, build := range builds {
				if !build.Terminal() {
					if err := r.backend.Abort(ctx, provider, credential, build.ID); err != nil {
						return err
					}
					waiting = true
				}
			}
			if waiting {
				return nil
			}
		}
		if childExists {
			if raw.Metadata.Annotations[ownerAnnotation] != v.Metadata.UID {
				return fmt.Errorf("refusing to delete unowned Pipeline")
			}
			if raw.Metadata.DeletionTimestamp == nil {
				rv, err := version(raw.Metadata)
				if err != nil {
					return err
				}
				return r.store.Delete(ctx, raw.Kind, raw.Metadata.Name, rv)
			}
			return nil
		}
		// Publication may have succeeded before its status write; cleanup is still safe by UID ownership.
		if _, err := r.publisher.Publish(ctx, gitpublication.PublishRequest{Repository: v.Status.Snapshot.Repository, PublicationName: v.Metadata.Name, Branch: v.Status.Snapshot.Branch, RootPath: name, OwnerUID: v.Metadata.UID, RemoveOwned: true}); err != nil {
			return err
		}
	}
	v.Metadata.Finalizers = slices.DeleteFunc(append([]string(nil), v.Metadata.Finalizers...), func(s string) bool { return s == cleanupFinalizer })
	encoded, err := v.Encode()
	if err != nil {
		return err
	}
	rv, err := version(v.Metadata)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, rv)
	return err
}
func (r *Reconciler) requestsMatching(ctx context.Context, match func(registry.Command) bool) ([]controller.Request, error) {
	items, err := r.store.List(ctx, registry.CommandResource.Kind)
	if err != nil {
		return nil, err
	}
	var requests []controller.Request
	for _, raw := range items.Items {
		v, err := registry.CommandResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if match(v) {
			requests = append(requests, controller.Request{Kind: v.Kind, Name: v.Metadata.Name})
		}
	}
	return requests, nil
}
func (r *Reconciler) RequestsForCommandsPipeline(ctx context.Context, q controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(v registry.Command) bool { return v.Spec.CommandsPipelineRef.Name == q.Name })
}
func (r *Reconciler) RequestsForCommand(_ context.Context, q controller.Request) ([]controller.Request, error) {
	return []controller.Request{q}, nil
}
func (r *Reconciler) RequestsForDependency(ctx context.Context, q controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(v registry.Command) bool { return !terminal(v) || v.Metadata.DeletionTimestamp != nil })
}
