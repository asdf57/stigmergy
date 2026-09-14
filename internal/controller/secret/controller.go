package secret

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/store"
	"github.com/asdf57/stigmergy/internal/utils"
)

const cleanupFinalizer = "homelab.io/secret-cleanup"

type Reconciler struct {
	store store.Store
}

func NewReconciler(store store.Store) *Reconciler {
	return &Reconciler{store: store}
}

func hasFinalizer(finalizers []string, finalizer string) bool {
	return slices.Contains(finalizers, finalizer)
}

func removeString(slice []string, s string) []string {
	for i, v := range slice {
		if v == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func (r *Reconciler) removeFinalizer(ctx context.Context, secretResource *registry.Secret) error {
	if !hasFinalizer(secretResource.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}

	secretResource.Metadata.Finalizers = removeString(secretResource.Metadata.Finalizers, cleanupFinalizer)
	raw, err := secretResource.Encode()
	if err != nil {
		return fmt.Errorf("encode Secret %q: %w", secretResource.Metadata.Name, err)
	}

	revision, err := strconv.ParseInt(
		secretResource.Metadata.ResourceVersion,
		10,
		64,
	)
	if err != nil {
		return fmt.Errorf("parse resource version for Secret %q: %w", secretResource.Metadata.Name, err)
	}

	_, err = r.store.Update(ctx, raw, revision)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}

	return err
}

func (r *Reconciler) finalize(ctx context.Context, secretResource *registry.Secret) error {
	fmt.Printf("[secret controller] Handling secret %s for deletion\n", secretResource.Metadata.Name)

	if !hasFinalizer(secretResource.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}

	rawSecretStore, err := r.store.Get(
		ctx,
		registry.SecretStoreResource.Kind,
		secretResource.Spec.SecretStoreRef.Name,
	)
	if errors.Is(err, store.ErrNotFound) {
		// The external value cannot be reached once its SecretStore is gone.
		// Release the finalizer so API cleanup is not blocked indefinitely.
		return r.removeFinalizer(ctx, secretResource)
	} else if err != nil {
		return fmt.Errorf("failed to get secret store %s for secret %s: %w", secretResource.Spec.SecretStoreRef.Name, secretResource.Metadata.Name, err)
	}

	secretStore, err := registry.SecretStoreResource.Decode(rawSecretStore)
	if err != nil {
		return fmt.Errorf("failed to decode secret store %s for secret %s: %w", secretResource.Spec.SecretStoreRef.Name, secretResource.Metadata.Name, err)
	}

	if provider := secretStore.Spec.Provider.OpenBao; provider != nil {
		client := utils.NewOpenBaoKeyStore(provider)
		if err := client.DeleteSecret(ctx, secretResource.Spec.Path); err != nil {
			return fmt.Errorf("delete OpenBao secret %q: %w",
				secretResource.Spec.Path, err)
		}
	}

	return r.removeFinalizer(ctx, secretResource)
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	fmt.Printf("[secret controller] Reconciling secret %s\n", request.Name)
	raw, err := r.store.Get(ctx, registry.SecretResource.Kind, request.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Secret and finalizer are gone, nothing to do!
			return nil
		}
		return fmt.Errorf("get Secret %q: %w", request.Name, err)
	}

	secretResource, err := registry.SecretResource.Decode(raw)
	if err != nil {
		return err
	}

	fmt.Printf("secret store reference is %s\n", secretResource.Spec.SecretStoreRef.Name)

	if secretResource.Metadata.DeletionTimestamp != nil {
		fmt.Printf("[secret controller] Secret %s is marked for deletion\n", secretResource.Metadata.Name)
		return r.finalize(ctx, &secretResource)
	}

	if secretResource.Status != nil &&
		secretResource.Status.Phase != nil && *secretResource.Status.Phase == apigen.SecretStatusPhaseReady &&
		secretResource.Status.ObservedGeneration != nil && *secretResource.Status.ObservedGeneration == secretResource.Metadata.Generation {
		return nil
	}

	// look up the secret store resource
	rawStore, err := r.store.Get(ctx, registry.SecretStoreResource.Kind, secretResource.Spec.SecretStoreRef.Name)
	if err != nil {
		return r.fail(ctx, secretResource, "SecretStoreUnavailable", fmt.Errorf("get SecretStore %q: %w", secretResource.Spec.SecretStoreRef.Name, err))
	}

	secretStoreResource, err := registry.SecretStoreResource.Decode(rawStore)
	if err != nil {
		return fmt.Errorf("failed to decode secret store resource: %w", err)
	}

	fmt.Printf("secret store name is %s\n", secretStoreResource.Metadata.Name)

	// Look up the provider type
	provider := secretStoreResource.Spec.Provider
	if provider.OpenBao != nil {
		fmt.Printf("Using OpenBao provider with address: %s\n", provider.OpenBao.Address)
		openBaoClient := utils.NewOpenBaoKeyStore(provider.OpenBao)
		if err := openBaoClient.WriteSecret(ctx, secretResource.Spec.Path, secretResource.Spec.Data); err != nil {
			return r.fail(ctx, secretResource, "WriteFailed", fmt.Errorf("write secret to OpenBao: %w", err))
		}
		return r.ready(ctx, secretResource)
	}

	return r.fail(ctx, secretResource, "UnsupportedProvider", errors.New("SecretStore does not configure a supported provider"))
}

func (r *Reconciler) ready(ctx context.Context, secretResource registry.Secret) error {
	phase, externalVersion := apigen.SecretStatusPhaseReady, int64(1)
	message, observedGeneration := "The secret is present in the external store", secretResource.Metadata.Generation
	conditions := []apigen.SecretCondition{{
		Type: "Ready", Status: apigen.SecretConditionStatusTrue, Reason: "SecretWritten",
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status := &apigen.SecretStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration, ExternalVersion: &externalVersion, Conditions: &conditions,
	}
	return r.updateStatus(ctx, secretResource, status)
}

func (r *Reconciler) fail(ctx context.Context, secretResource registry.Secret, reason string, reconcileErr error) error {
	phase := apigen.SecretStatusPhaseFailed
	message, observedGeneration := reconcileErr.Error(), secretResource.Metadata.Generation
	conditions := []apigen.SecretCondition{{
		Type: "Ready", Status: apigen.SecretConditionStatusFalse, Reason: reason,
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status := &apigen.SecretStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration, Conditions: &conditions,
	}
	if err := r.updateStatus(ctx, secretResource, status); err != nil {
		return err
	}
	return reconcileErr
}

func (r *Reconciler) updateStatus(ctx context.Context, secretResource registry.Secret, status *apigen.SecretStatus) error {
	revision, err := strconv.ParseInt(secretResource.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse resource version for Secret %q: %w", secretResource.Metadata.Name, err)
	}
	storedStatus, err := registry.SecretResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	if _, err := r.store.UpdateStatus(ctx, secretResource.Kind, secretResource.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update Secret %q status: %w", secretResource.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) RequestsForSecretStore(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	list, err := r.store.List(ctx, registry.SecretResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list Secrets for SecretStore %q: %w", request.Name, err)
	}
	requests := make([]controller.Request, 0)
	for _, raw := range list.Items {
		secretResource, err := registry.SecretResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode Secret %q: %w", raw.Metadata.Name, err)
		}
		if secretResource.Spec.SecretStoreRef.Name == request.Name {
			requests = append(requests, controller.Request{Kind: secretResource.Kind, Name: secretResource.Metadata.Name})
		}
	}
	return requests, nil
}
