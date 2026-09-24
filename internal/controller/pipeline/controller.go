package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const cleanupFinalizer = "homelab.io/pipeline-cleanup"

type Reconciler struct {
	store   store.Store
	backend Backend
}

func NewReconciler(resourceStore store.Store) *Reconciler {
	return NewReconcilerWithBackend(resourceStore, NewFlyBackend())
}
func NewReconcilerWithBackend(resourceStore store.Store, backend Backend) *Reconciler {
	return &Reconciler{store: resourceStore, backend: backend}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Pipeline %q: %w", request.Name, err)
	}
	value, err := registry.PipelineResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode Pipeline %q: %w", request.Name, err)
	}
	if value.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, value)
	}
	provider, credential, phase, reason, message, err := r.resolve(ctx, value)
	if err != nil {
		return err
	}
	if phase != "" {
		return r.updateStatus(ctx, value, pipelineStatus(value, apigen.PipelineStatusPhase(phase), reason, message, ""))
	}
	if value.Spec.Definition.Format != apigen.PipelineDefinitionFormatConcourse {
		return r.updateStatus(ctx, value, pipelineStatus(value, apigen.PipelineStatusPhaseFailed, "UnsupportedDefinition", fmt.Sprintf("pipeline format %q is not supported", value.Spec.Definition.Format), ""))
	}
	if value.Status != nil && value.Status.ExternalName != nil && *value.Status.ExternalName != "" && *value.Status.ExternalName != value.Spec.ExternalName {
		if err := r.backend.Delete(ctx, provider, credential, *value.Status.ExternalName); err != nil {
			return r.backendFailure(ctx, value, "RenameCleanupFailed", err)
		}
	}
	digestBytes := sha256.Sum256([]byte(value.Spec.Definition.Data))
	digest := hex.EncodeToString(digestBytes[:])
	if pipelineApplied(value, digest) {
		return nil
	}
	if err := r.backend.Apply(ctx, provider, credential, value.Spec.ExternalName, value.Spec.Definition.Data); err != nil {
		return r.backendFailure(ctx, value, "ApplyFailed", err)
	}
	return r.updateStatus(ctx, value, pipelineStatus(value, apigen.PipelineStatusPhaseReady, "PipelineApplied", "The external pipeline is configured and unpaused", digest))
}

func (r *Reconciler) resolve(ctx context.Context, value registry.Pipeline) (registry.PipelineProvider, registry.UsernamePasswordCredential, string, string, string, error) {
	raw, err := r.store.Get(ctx, registry.PipelineProviderResource.Kind, value.Spec.ProviderRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, "Pending", "ProviderNotFound", fmt.Sprintf("PipelineProvider %q does not exist", value.Spec.ProviderRef.Name), nil
	}
	if err != nil {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, "", "", "", err
	}
	provider, err := registry.PipelineProviderResource.Decode(raw)
	if err != nil {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, "Failed", "ProviderInvalid", err.Error(), nil
	}
	if provider.Status == nil || provider.Status.Phase == nil || *provider.Status.Phase != apigen.PipelineProviderStatusPhaseReady || provider.Status.ObservedGeneration == nil || *provider.Status.ObservedGeneration != provider.Metadata.Generation {
		return provider, registry.UsernamePasswordCredential{}, "Pending", "ProviderNotReady", fmt.Sprintf("PipelineProvider %q is not Ready", provider.Metadata.Name), nil
	}
	raw, err = r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, provider.Spec.CredentialRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return provider, registry.UsernamePasswordCredential{}, "Pending", "CredentialNotFound", fmt.Sprintf("UsernamePasswordCredential %q does not exist", provider.Spec.CredentialRef.Name), nil
	}
	if err != nil {
		return provider, registry.UsernamePasswordCredential{}, "", "", "", err
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(raw)
	if err != nil {
		return provider, registry.UsernamePasswordCredential{}, "Failed", "CredentialInvalid", err.Error(), nil
	}
	return provider, credential, "", "", "", nil
}

func pipelineApplied(value registry.Pipeline, digest string) bool {
	return value.Status != nil && value.Status.Phase != nil && *value.Status.Phase == apigen.PipelineStatusPhaseReady && value.Status.ObservedGeneration != nil && *value.Status.ObservedGeneration == value.Metadata.Generation && value.Status.AppliedDigest != nil && *value.Status.AppliedDigest == digest && value.Status.ExternalName != nil && *value.Status.ExternalName == value.Spec.ExternalName
}

func pipelineStatus(value registry.Pipeline, phase apigen.PipelineStatusPhase, reason, message, digest string) *apigen.PipelineStatus {
	generation, externalName := value.Metadata.Generation, value.Spec.ExternalName
	conditionStatus := apigen.PipelineConditionStatusFalse
	if phase == apigen.PipelineStatusPhaseReady {
		conditionStatus = apigen.PipelineConditionStatusTrue
	}
	conditions := []apigen.PipelineCondition{{Type: "Ready", Status: conditionStatus, Reason: reason, Message: &message, ObservedGeneration: &generation}}
	status := &apigen.PipelineStatus{Phase: &phase, ObservedGeneration: &generation, ExternalName: &externalName, Conditions: &conditions}
	if digest != "" {
		status.AppliedDigest = &digest
	}
	return status
}

func (r *Reconciler) backendFailure(ctx context.Context, value registry.Pipeline, reason string, backendErr error) error {
	if err := r.updateStatus(ctx, value, pipelineStatus(value, apigen.PipelineStatusPhaseFailed, reason, backendErr.Error(), "")); err != nil {
		return err
	}
	return backendErr
}

func (r *Reconciler) updateStatus(ctx context.Context, value registry.Pipeline, status *apigen.PipelineStatus) error {
	if resource.EqualJSON(value.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	encoded, err := registry.PipelineResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, value.Kind, value.Metadata.Name, encoded, revision)
	return err
}

func (r *Reconciler) finalize(ctx context.Context, value registry.Pipeline) error {
	if !slices.Contains(value.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	provider, credential, found, err := r.resolveForDelete(ctx, value)
	if err != nil {
		return err
	}
	if found {
		name := value.Spec.ExternalName
		if value.Status != nil && value.Status.ExternalName != nil && *value.Status.ExternalName != "" {
			name = *value.Status.ExternalName
		}
		if err := r.backend.Delete(ctx, provider, credential, name); err != nil {
			return err
		}
	}
	value.Metadata.Finalizers = slices.DeleteFunc(value.Metadata.Finalizers, func(item string) bool { return item == cleanupFinalizer })
	encoded, err := value.Encode()
	if err != nil {
		return err
	}
	revision, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, revision)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (r *Reconciler) resolveForDelete(ctx context.Context, value registry.Pipeline) (registry.PipelineProvider, registry.UsernamePasswordCredential, bool, error) {
	raw, err := r.store.Get(ctx, registry.PipelineProviderResource.Kind, value.Spec.ProviderRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, false, nil
	}
	if err != nil {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, false, err
	}
	provider, err := registry.PipelineProviderResource.Decode(raw)
	if err != nil {
		return registry.PipelineProvider{}, registry.UsernamePasswordCredential{}, false, err
	}
	raw, err = r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, provider.Spec.CredentialRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return provider, registry.UsernamePasswordCredential{}, false, nil
	}
	if err != nil {
		return provider, registry.UsernamePasswordCredential{}, false, err
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(raw)
	if err != nil {
		return provider, registry.UsernamePasswordCredential{}, false, err
	}
	return provider, credential, true, nil
}

func (r *Reconciler) RequestsForProvider(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(value registry.Pipeline) bool { return value.Spec.ProviderRef.Name == request.Name })
}
func (r *Reconciler) RequestsForCredential(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	providers, err := r.store.List(ctx, registry.PipelineProviderResource.Kind)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, raw := range providers.Items {
		provider, err := registry.PipelineProviderResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if provider.Spec.CredentialRef.Name == request.Name {
			names[provider.Metadata.Name] = true
		}
	}
	return r.requestsMatching(ctx, func(value registry.Pipeline) bool { return names[value.Spec.ProviderRef.Name] })
}
func (r *Reconciler) requestsMatching(ctx context.Context, match func(registry.Pipeline) bool) ([]controller.Request, error) {
	items, err := r.store.List(ctx, registry.PipelineResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := make([]controller.Request, 0)
	for _, raw := range items.Items {
		value, err := registry.PipelineResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if match(value) {
			requests = append(requests, controller.Request{Kind: value.Kind, Name: value.Metadata.Name})
		}
	}
	return requests, nil
}
