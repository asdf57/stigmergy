package usernamepassword

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const (
	cleanupFinalizer      = "homelab.io/username-password-cleanup"
	secretFinalizer       = "homelab.io/secret-cleanup"
	ownerUIDAnnotation    = "homelab.io/username-password-uid"
	ownerUIDDataKey       = "usernamePasswordUid"
	defaultSecretBasePath = "username-password-creds"
)

var errSecretOwnershipConflict = errors.New("Secret ownership conflict")

type Reconciler struct{ store store.Store }

func NewReconciler(resourceStore store.Store) *Reconciler {
	return &Reconciler{store: resourceStore}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get UsernamePasswordCredential %q: %w", request.Name, err)
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode UsernamePasswordCredential %q: %w", request.Name, err)
	}
	if credential.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, credential)
	}

	secretResource, changed, err := r.ensureSecret(ctx, credential)
	if err != nil {
		phase, reason := "Failed", "SecretProvisioningFailed"
		if errors.Is(err, errSecretOwnershipConflict) {
			phase, reason = "Conflict", "SecretOwnershipConflict"
		}
		return r.updateStatus(ctx, credential, failureStatus(credential, phase, reason, err.Error()))
	}
	if changed || !secretReady(secretResource) {
		if secretResource.Status != nil && secretResource.Status.Phase != nil && *secretResource.Status.Phase == apigen.SecretStatusPhaseFailed {
			return r.updateStatus(ctx, credential, failureStatus(credential, "Failed", "SecretReconciliationFailed", fmt.Sprintf("Secret %q failed reconciliation", secretResource.Metadata.Name)))
		}
		return r.updateStatus(ctx, credential, failureStatus(credential, "Pending", "SecretPending", fmt.Sprintf("Secret %q is waiting for reconciliation", secretResource.Metadata.Name)))
	}

	phase := apigen.UsernamePasswordCredentialStatusPhaseReady
	message := "The username and password are managed by a Ready Secret resource"
	observedGeneration := credential.Metadata.Generation
	conditions := []apigen.UsernamePasswordCredentialCondition{{
		Type: "Ready", Status: apigen.UsernamePasswordCredentialConditionStatusTrue, Reason: "CredentialsAvailable",
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status := &apigen.UsernamePasswordCredentialStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration,
		SecretRef: &apigen.ResourceReference{Name: secretResource.Metadata.Name, Uid: secretResource.Metadata.UID}, Conditions: &conditions,
	}
	return r.updateStatus(ctx, credential, status)
}

func (r *Reconciler) ensureSecret(ctx context.Context, credential registry.UsernamePasswordCredential) (registry.Secret, bool, error) {
	raw, err := r.store.Get(ctx, registry.SecretResource.Kind, credential.Metadata.Name)
	if err == nil {
		secretResource, decodeErr := registry.SecretResource.Decode(raw)
		if decodeErr != nil {
			return registry.Secret{}, false, fmt.Errorf("decode Secret %q: %w", credential.Metadata.Name, decodeErr)
		}
		if validateErr := validateOwnedSecret(secretResource, credential); validateErr != nil {
			return secretResource, false, validateErr
		}
		return r.updateSecretIfNeeded(ctx, secretResource, credential)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return registry.Secret{}, false, fmt.Errorf("get Secret %q: %w", credential.Metadata.Name, err)
	}

	desired := desiredSecret(credential, resource.Metadata{
		Name: credential.Metadata.Name, Finalizers: []string{secretFinalizer},
		Annotations: map[string]string{ownerUIDAnnotation: credential.Metadata.UID},
	})
	encoded, err := desired.Encode()
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("encode Secret %q: %w", credential.Metadata.Name, err)
	}
	created, err := r.store.Create(ctx, encoded)
	if errors.Is(err, store.ErrConflict) {
		return r.ensureSecret(ctx, credential)
	}
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("create Secret %q: %w", credential.Metadata.Name, err)
	}
	secretResource, err := registry.SecretResource.Decode(created)
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("decode created Secret %q: %w", credential.Metadata.Name, err)
	}
	return secretResource, true, nil
}

func (r *Reconciler) updateSecretIfNeeded(ctx context.Context, secretResource registry.Secret, credential registry.UsernamePasswordCredential) (registry.Secret, bool, error) {
	desired := desiredSecret(credential, secretResource.Metadata)
	if resource.EqualJSON(secretResource.Spec, desired.Spec) && slices.Contains(secretResource.Metadata.Finalizers, secretFinalizer) {
		return secretResource, false, nil
	}
	if !slices.Contains(desired.Metadata.Finalizers, secretFinalizer) {
		desired.Metadata.Finalizers = append(desired.Metadata.Finalizers, secretFinalizer)
	}
	encoded, err := desired.Encode()
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("encode Secret %q: %w", credential.Metadata.Name, err)
	}
	revision, err := strconv.ParseInt(secretResource.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("parse Secret %q resource version: %w", secretResource.Metadata.Name, err)
	}
	updated, err := r.store.Update(ctx, encoded, revision)
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("update Secret %q: %w", secretResource.Metadata.Name, err)
	}
	decoded, err := registry.SecretResource.Decode(updated)
	if err != nil {
		return registry.Secret{}, false, fmt.Errorf("decode updated Secret %q: %w", secretResource.Metadata.Name, err)
	}
	return decoded, true, nil
}

func desiredSecret(credential registry.UsernamePasswordCredential, metadata resource.Metadata) registry.Secret {
	secretPath := credential.Spec.Path
	if secretPath == "" || secretPath == defaultSecretBasePath {
		secretPath = path.Join(defaultSecretBasePath, credential.Metadata.Name)
	}
	return registry.NewSecret(metadata, apigen.SecretSpec{
		SecretStoreRef: apigen.SecretStoreReference{Name: credential.Spec.SecretStoreRef.Name},
		Path:           secretPath,
		Data: map[string]string{
			"username": credential.Spec.Username, "password": credential.Spec.Password, ownerUIDDataKey: credential.Metadata.UID,
		},
	})
}

func validateOwnedSecret(secretResource registry.Secret, credential registry.UsernamePasswordCredential) error {
	if secretResource.Metadata.Annotations[ownerUIDAnnotation] != credential.Metadata.UID {
		return fmt.Errorf("%w: Secret %q is not owned by UsernamePasswordCredential %q", errSecretOwnershipConflict, secretResource.Metadata.Name, credential.Metadata.Name)
	}
	if secretResource.Metadata.DeletionTimestamp != nil {
		return fmt.Errorf("Secret %q is terminating", secretResource.Metadata.Name)
	}
	if secretResource.Spec.Data[ownerUIDDataKey] != credential.Metadata.UID {
		return fmt.Errorf("%w: Secret %q has unexpected ownership data", errSecretOwnershipConflict, secretResource.Metadata.Name)
	}
	return nil
}

func secretReady(secretResource registry.Secret) bool {
	return secretResource.Status != nil && secretResource.Status.Phase != nil &&
		*secretResource.Status.Phase == apigen.SecretStatusPhaseReady && secretResource.Status.ObservedGeneration != nil &&
		*secretResource.Status.ObservedGeneration == secretResource.Metadata.Generation
}

func failureStatus(credential registry.UsernamePasswordCredential, phase, reason, message string) *apigen.UsernamePasswordCredentialStatus {
	statusPhase := apigen.UsernamePasswordCredentialStatusPhase(phase)
	observedGeneration := credential.Metadata.Generation
	conditions := []apigen.UsernamePasswordCredentialCondition{{
		Type: "Ready", Status: apigen.UsernamePasswordCredentialConditionStatusFalse, Reason: reason,
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	return &apigen.UsernamePasswordCredentialStatus{
		Phase: &statusPhase, ObservedGeneration: &observedGeneration, Conditions: &conditions,
	}
}

func (r *Reconciler) updateStatus(ctx context.Context, credential registry.UsernamePasswordCredential, status *apigen.UsernamePasswordCredentialStatus) error {
	if resource.EqualJSON(credential.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(credential.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse UsernamePasswordCredential %q resource version: %w", credential.Metadata.Name, err)
	}
	storedStatus, err := registry.UsernamePasswordCredentialResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	if _, err := r.store.UpdateStatus(ctx, credential.Kind, credential.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update UsernamePasswordCredential %q status: %w", credential.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) finalize(ctx context.Context, credential registry.UsernamePasswordCredential) error {
	if !slices.Contains(credential.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	raw, err := r.store.Get(ctx, registry.SecretResource.Kind, credential.Metadata.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.removeFinalizer(ctx, credential)
	}
	if err != nil {
		return fmt.Errorf("get Secret %q while finalizing UsernamePasswordCredential: %w", credential.Metadata.Name, err)
	}
	secretResource, err := registry.SecretResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode Secret %q while finalizing UsernamePasswordCredential: %w", credential.Metadata.Name, err)
	}
	if secretResource.Metadata.Annotations[ownerUIDAnnotation] != credential.Metadata.UID {
		return fmt.Errorf("%w: refusing to delete Secret %q", errSecretOwnershipConflict, secretResource.Metadata.Name)
	}
	if secretResource.Metadata.DeletionTimestamp != nil {
		return nil
	}
	revision, err := strconv.ParseInt(secretResource.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse Secret %q resource version: %w", secretResource.Metadata.Name, err)
	}
	if err := r.store.Delete(ctx, secretResource.Kind, secretResource.Metadata.Name, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("delete Secret %q while finalizing UsernamePasswordCredential: %w", secretResource.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, credential registry.UsernamePasswordCredential) error {
	credential.Metadata.Finalizers = slices.DeleteFunc(credential.Metadata.Finalizers, func(value string) bool { return value == cleanupFinalizer })
	encoded, err := credential.Encode()
	if err != nil {
		return fmt.Errorf("encode UsernamePasswordCredential %q while removing finalizer: %w", credential.Metadata.Name, err)
	}
	revision, err := strconv.ParseInt(credential.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse UsernamePasswordCredential %q resource version: %w", credential.Metadata.Name, err)
	}
	if _, err := r.store.Update(ctx, encoded, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove finalizer from UsernamePasswordCredential %q: %w", credential.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) RequestsForSecret(_ context.Context, request controller.Request) ([]controller.Request, error) {
	return []controller.Request{{Kind: registry.UsernamePasswordCredentialResource.Kind, Name: request.Name}}, nil
}
