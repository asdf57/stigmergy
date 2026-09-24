package command

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/controller/commandspipeline"
	"github.com/asdf57/stigmergy/internal/controller/publication"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

type Reconciler struct {
	store     store.Store
	publisher publication.Publisher
}

func NewReconciler(resourceStore store.Store) *Reconciler {
	return &Reconciler{store: resourceStore, publisher: publication.NewGitPublisher(resourceStore)}
}

func NewReconcilerWithPublisher(resourceStore store.Store, publisher publication.Publisher) *Reconciler {
	return &Reconciler{store: resourceStore, publisher: publisher}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.CommandResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Command %q: %w", request.Name, err)
	}
	value, err := registry.CommandResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode Command %q: %w", request.Name, err)
	}

	conflict, err := r.conflictingCommand(ctx, value)
	if err != nil {
		return err
	}
	if conflict != "" {
		return r.setStatus(ctx, value, apigen.CommandStatusPhaseConflict, "TargetOwnershipConflict",
			fmt.Sprintf("Command %q already targets InventoryCaptureGroup %q", conflict, value.Spec.InventoryCaptureGroupRef.Name), nil, nil)
	}

	pipelines, err := r.pipelinesForCaptureGroup(ctx, value.Spec.InventoryCaptureGroupRef.Name)
	if err != nil {
		return err
	}
	if len(pipelines) == 0 {
		return r.setStatus(ctx, value, apigen.CommandStatusPhasePending, "CommandsPipelineNotFound",
			fmt.Sprintf("no CommandsPipeline targets InventoryCaptureGroup %q", value.Spec.InventoryCaptureGroupRef.Name), nil, nil)
	}
	if len(pipelines) > 1 {
		return r.setStatus(ctx, value, apigen.CommandStatusPhaseConflict, "CommandsPipelineConflict",
			fmt.Sprintf("multiple CommandsPipelines target InventoryCaptureGroup %q", value.Spec.InventoryCaptureGroupRef.Name), nil, nil)
	}
	pipeline := pipelines[0]
	pipelineRef := &apigen.ResourceReference{Name: pipeline.Metadata.Name, Uid: pipeline.Metadata.UID}
	if pipeline.Status == nil || pipeline.Status.Phase == nil || *pipeline.Status.Phase != apigen.CommandsPipelineStatusPhaseReady ||
		pipeline.Status.ObservedGeneration == nil || *pipeline.Status.ObservedGeneration != pipeline.Metadata.Generation {
		return r.setStatus(ctx, value, apigen.CommandStatusPhasePending, "CommandsPipelineNotReady",
			fmt.Sprintf("CommandsPipeline %q is not Ready", pipeline.Metadata.Name), pipelineRef, nil)
	}

	repositoryRaw, err := r.store.Get(ctx, registry.GitRepositoryResource.Kind, pipeline.Spec.CommandsRepositoryRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.setStatus(ctx, value, apigen.CommandStatusPhasePending, "RepositoryNotFound",
			fmt.Sprintf("GitRepository %q does not exist", pipeline.Spec.CommandsRepositoryRef.Name), pipelineRef, nil)
	}
	if err != nil {
		return fmt.Errorf("get GitRepository %q: %w", pipeline.Spec.CommandsRepositoryRef.Name, err)
	}
	repository, err := registry.GitRepositoryResource.Decode(repositoryRaw)
	if err != nil {
		return r.setStatus(ctx, value, apigen.CommandStatusPhaseFailed, "RepositoryInvalid", err.Error(), pipelineRef, nil)
	}
	commandPath, err := commandspipeline.SafeCommandPath(pipeline)
	if err != nil {
		return r.setStatus(ctx, value, apigen.CommandStatusPhaseFailed, "InvalidCommandPath", err.Error(), pipelineRef, nil)
	}

	result, err := r.publisher.Publish(ctx, publication.PublishRequest{
		Repository:        repository.Spec,
		PublicationName:   value.Metadata.Name,
		Branch:            value.Spec.InventoryCaptureGroupRef.Name,
		RootPath:          path.Dir(commandPath),
		Artifacts:         []publication.Artifact{{Path: path.Base(commandPath), Content: []byte(value.Spec.Script)}},
		PreserveUnmanaged: true,
	})
	if err != nil {
		return r.setStatus(ctx, value, apigen.CommandStatusPhaseFailed, "PublicationFailed", err.Error(), pipelineRef, nil)
	}
	return r.setStatus(ctx, value, apigen.CommandStatusPhasePublished, "CommandPublished",
		fmt.Sprintf("Published %s to branch %s", commandPath, value.Spec.InventoryCaptureGroupRef.Name), pipelineRef, &result.Revision)
}

func (r *Reconciler) conflictingCommand(ctx context.Context, value registry.Command) (string, error) {
	items, err := r.store.List(ctx, registry.CommandResource.Kind)
	if err != nil {
		return "", fmt.Errorf("list Commands for target ownership: %w", err)
	}
	for _, raw := range items.Items {
		other, err := registry.CommandResource.Decode(raw)
		if err != nil {
			return "", fmt.Errorf("decode Command %q: %w", raw.Metadata.Name, err)
		}
		if other.Metadata.Name != value.Metadata.Name && other.Spec.InventoryCaptureGroupRef.Name == value.Spec.InventoryCaptureGroupRef.Name {
			return other.Metadata.Name, nil
		}
	}
	return "", nil
}

func (r *Reconciler) pipelinesForCaptureGroup(ctx context.Context, name string) ([]registry.CommandsPipeline, error) {
	items, err := r.store.List(ctx, registry.CommandsPipelineResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list CommandsPipelines: %w", err)
	}
	values := make([]registry.CommandsPipeline, 0, 1)
	for _, raw := range items.Items {
		value, err := registry.CommandsPipelineResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode CommandsPipeline %q: %w", raw.Metadata.Name, err)
		}
		if value.Spec.InventoryCaptureGroupRef.Name == name {
			values = append(values, value)
		}
	}
	return values, nil
}

func (r *Reconciler) setStatus(ctx context.Context, value registry.Command, phase apigen.CommandStatusPhase, reason, message string, pipelineRef *apigen.ResourceReference, revision *string) error {
	generation := value.Metadata.Generation
	conditionStatus := apigen.CommandConditionStatusFalse
	if phase == apigen.CommandStatusPhasePublished {
		conditionStatus = apigen.CommandConditionStatusTrue
	}
	conditions := []apigen.CommandCondition{{
		Type: "Ready", Status: conditionStatus, Reason: reason, Message: &message, ObservedGeneration: &generation,
	}}
	status := &apigen.CommandStatus{
		Phase: &phase, ObservedGeneration: &generation, CommandsPipelineRef: pipelineRef, Revision: revision, Conditions: &conditions,
	}
	if resource.EqualJSON(value.Status, status) {
		return nil
	}
	resourceVersion, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse Command %q resource version: %w", value.Metadata.Name, err)
	}
	encoded, err := registry.CommandResource.EncodeStatus(status)
	if err != nil {
		return fmt.Errorf("encode Command %q status: %w", value.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, value.Kind, value.Metadata.Name, encoded, resourceVersion); err != nil {
		return fmt.Errorf("update Command %q status: %w", value.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) requestsMatching(ctx context.Context, matches func(registry.Command) bool) ([]controller.Request, error) {
	items, err := r.store.List(ctx, registry.CommandResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := make([]controller.Request, 0, len(items.Items))
	for _, raw := range items.Items {
		value, err := registry.CommandResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if matches(value) {
			requests = append(requests, controller.Request{Kind: value.Kind, Name: value.Metadata.Name})
		}
	}
	return requests, nil
}

func (r *Reconciler) RequestsForCommandsPipeline(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	raw, err := r.store.Get(ctx, registry.CommandsPipelineResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.requestsMatching(ctx, func(registry.Command) bool { return true })
	}
	if err != nil {
		return nil, err
	}
	pipeline, err := registry.CommandsPipelineResource.Decode(raw)
	if err != nil {
		return nil, err
	}
	return r.requestsMatching(ctx, func(value registry.Command) bool {
		return value.Spec.InventoryCaptureGroupRef.Name == pipeline.Spec.InventoryCaptureGroupRef.Name
	})
}

func (r *Reconciler) RequestsForCommand(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(registry.Command) bool { return true })
}

func (r *Reconciler) RequestsForGitRepository(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	pipelines, err := r.store.List(ctx, registry.CommandsPipelineResource.Kind)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]struct{})
	for _, raw := range pipelines.Items {
		pipeline, err := registry.CommandsPipelineResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if pipeline.Spec.CommandsRepositoryRef.Name == request.Name {
			groups[pipeline.Spec.InventoryCaptureGroupRef.Name] = struct{}{}
		}
	}
	return r.requestsMatching(ctx, func(value registry.Command) bool {
		_, found := groups[value.Spec.InventoryCaptureGroupRef.Name]
		return found
	})
}

func (r *Reconciler) RequestsForSSHKeyPair(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(registry.Command) bool { return true })
}
