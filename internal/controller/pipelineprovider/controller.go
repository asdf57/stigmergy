package pipelineprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

type Reconciler struct{ store store.Store }

func NewReconciler(resourceStore store.Store) *Reconciler { return &Reconciler{store: resourceStore} }

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.PipelineProviderResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get PipelineProvider %q: %w", request.Name, err)
	}
	provider, err := registry.PipelineProviderResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode PipelineProvider %q: %w", request.Name, err)
	}
	if provider.Spec.Type != apigen.PipelineProviderSpecTypeConcourse {
		return r.updateStatus(ctx, provider, providerStatus(provider, apigen.PipelineProviderStatusPhaseFailed, "UnsupportedProvider", fmt.Sprintf("pipeline provider type %q is not supported", provider.Spec.Type)))
	}
	parsed, err := url.ParseRequestURI(provider.Spec.Url)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return r.updateStatus(ctx, provider, providerStatus(provider, apigen.PipelineProviderStatusPhaseFailed, "InvalidURL", fmt.Sprintf("provider URL %q is invalid", provider.Spec.Url)))
	}
	credentialRaw, err := r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, provider.Spec.CredentialRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.updateStatus(ctx, provider, providerStatus(provider, apigen.PipelineProviderStatusPhasePending, "CredentialNotFound", fmt.Sprintf("UsernamePasswordCredential %q does not exist", provider.Spec.CredentialRef.Name)))
	}
	if err != nil {
		return fmt.Errorf("get credential for PipelineProvider %q: %w", provider.Metadata.Name, err)
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(credentialRaw)
	if err != nil {
		return r.updateStatus(ctx, provider, providerStatus(provider, apigen.PipelineProviderStatusPhaseFailed, "CredentialInvalid", err.Error()))
	}
	if !credentialReady(credential) {
		return r.updateStatus(ctx, provider, providerStatus(provider, apigen.PipelineProviderStatusPhasePending, "CredentialNotReady", fmt.Sprintf("UsernamePasswordCredential %q is not Ready", credential.Metadata.Name)))
	}
	return r.updateStatus(ctx, provider, providerStatus(provider, apigen.PipelineProviderStatusPhaseReady, "ProviderReady", "The pipeline provider and its credential are Ready"))
}

func credentialReady(value registry.UsernamePasswordCredential) bool {
	return value.Status != nil && value.Status.Phase != nil && *value.Status.Phase == apigen.UsernamePasswordCredentialStatusPhaseReady && value.Status.ObservedGeneration != nil && *value.Status.ObservedGeneration == value.Metadata.Generation
}

func providerStatus(value registry.PipelineProvider, phase apigen.PipelineProviderStatusPhase, reason, message string) *apigen.PipelineProviderStatus {
	generation := value.Metadata.Generation
	conditionStatus := apigen.PipelineProviderConditionStatusFalse
	if phase == apigen.PipelineProviderStatusPhaseReady {
		conditionStatus = apigen.PipelineProviderConditionStatusTrue
	}
	conditions := []apigen.PipelineProviderCondition{{Type: "Ready", Status: conditionStatus, Reason: reason, Message: &message, ObservedGeneration: &generation}}
	return &apigen.PipelineProviderStatus{Phase: &phase, ObservedGeneration: &generation, Conditions: &conditions}
}

func (r *Reconciler) updateStatus(ctx context.Context, value registry.PipelineProvider, status *apigen.PipelineProviderStatus) error {
	if resource.EqualJSON(value.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse PipelineProvider %q resource version: %w", value.Metadata.Name, err)
	}
	encoded, err := registry.PipelineProviderResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, value.Kind, value.Metadata.Name, encoded, revision)
	if err != nil {
		return fmt.Errorf("update PipelineProvider %q status: %w", value.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) RequestsForCredential(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	items, err := r.store.List(ctx, registry.PipelineProviderResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list PipelineProviders: %w", err)
	}
	requests := make([]controller.Request, 0)
	for _, raw := range items.Items {
		value, err := registry.PipelineProviderResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if value.Spec.CredentialRef.Name == request.Name {
			requests = append(requests, controller.Request{Kind: value.Kind, Name: value.Metadata.Name})
		}
	}
	return requests, nil
}
