package sshkeypair

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"golang.org/x/crypto/ssh"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshkey"
	"github.com/asdf57/stigmergy/internal/store"
)

const (
	cleanupFinalizer   = "homelab.io/ssh-key-pair-cleanup"
	secretFinalizer    = "homelab.io/secret-cleanup"
	ownerUIDAnnotation = "homelab.io/ssh-key-pair-uid"
)

var errSecretOwnershipConflict = errors.New("Secret ownership conflict")

type Reconciler struct{ store store.Store }

func NewReconciler(resourceStore store.Store) *Reconciler {
	return &Reconciler{store: resourceStore}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	if request.Kind == registry.ServerResource.Kind {
		raw, err := r.store.Get(ctx, registry.ServerResource.Kind, request.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get Server %q while resolving SSH keys: %w", request.Name, err)
		}
		server, err := registry.ServerResource.Decode(raw)
		if err != nil {
			return fmt.Errorf("decode Server %q while resolving SSH keys: %w", request.Name, err)
		}
		return r.reconcileServerKeys(ctx, server)
	}

	raw, err := r.store.Get(ctx, registry.SSHKeyPairResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get SSHKeyPair %q: %w", request.Name, err)
	}
	keyPair, err := registry.SSHKeyPairResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode SSHKeyPair %q: %w", request.Name, err)
	}
	if keyPair.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, keyPair)
	}

	secretResource, publicKey, fingerprint, created, err := r.ensureSecret(ctx, keyPair)
	if err != nil {
		phase, reason := "Failed", "SecretProvisioningFailed"
		if errors.Is(err, errSecretOwnershipConflict) {
			phase, reason = "Conflict", "SecretOwnershipConflict"
		}
		return r.updateStatus(ctx, keyPair, failureStatus(keyPair, phase, reason, err.Error()))
	}
	if created || !secretReady(secretResource) {
		if secretResource.Status != nil && secretResource.Status.Phase != nil && *secretResource.Status.Phase == apigen.SecretStatusPhaseFailed {
			return r.updateStatus(ctx, keyPair, failureStatus(keyPair, "Failed", "SecretReconciliationFailed", fmt.Sprintf("Secret %q failed reconciliation", secretResource.Metadata.Name)))
		}
		return r.updateStatus(ctx, keyPair, failureStatus(keyPair, "Pending", "SecretPending", fmt.Sprintf("Secret %q is waiting for reconciliation", secretResource.Metadata.Name)))
	}

	phase, message, observedGeneration := apigen.SSHKeyPairStatusPhaseReady, "The generated key pair is managed by a Ready Secret resource", keyPair.Metadata.Generation
	conditions := []apigen.SSHKeyPairCondition{{
		Type: "Ready", Status: apigen.SSHKeyPairConditionStatusTrue, Reason: "KeyPairAvailable",
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status := &apigen.SSHKeyPairStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration, PublicKey: &publicKey, Fingerprint: &fingerprint,
		SecretRef: &apigen.ResourceReference{Name: secretResource.Metadata.Name, Uid: secretResource.Metadata.UID}, Conditions: &conditions,
	}
	return r.updateStatus(ctx, keyPair, status)
}

func (r *Reconciler) ensureSecret(ctx context.Context, keyPair registry.SSHKeyPair) (registry.Secret, string, string, bool, error) {
	raw, err := r.store.Get(ctx, registry.SecretResource.Kind, keyPair.Metadata.Name)
	if err == nil {
		secretResource, decodeErr := registry.SecretResource.Decode(raw)
		if decodeErr != nil {
			return registry.Secret{}, "", "", false, fmt.Errorf("decode Secret %q: %w", keyPair.Metadata.Name, decodeErr)
		}
		publicKey, fingerprint, validateErr := validateOwnedSecret(secretResource, keyPair)
		return secretResource, publicKey, fingerprint, false, validateErr
	}
	if !errors.Is(err, store.ErrNotFound) {
		return registry.Secret{}, "", "", false, fmt.Errorf("get Secret %q: %w", keyPair.Metadata.Name, err)
	}

	privateKey, publicKey, fingerprint, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		return registry.Secret{}, "", "", false, err
	}
	desired := registry.NewSecret(resource.Metadata{
		Name: keyPair.Metadata.Name, Finalizers: []string{secretFinalizer},
		Annotations: map[string]string{ownerUIDAnnotation: keyPair.Metadata.UID},
	}, apigen.SecretSpec{
		SecretStoreRef: apigen.SecretStoreReference{Name: keyPair.Spec.SecretStoreRef.Name},
		Path:           keyPair.Spec.Path,
		Data: map[string]string{
			"privateKey": privateKey, "publicKey": publicKey, "sshKeyPairUID": keyPair.Metadata.UID,
		},
	})
	encoded, err := desired.Encode()
	if err != nil {
		return registry.Secret{}, "", "", false, fmt.Errorf("encode Secret %q: %w", keyPair.Metadata.Name, err)
	}
	created, err := r.store.Create(ctx, encoded)
	if errors.Is(err, store.ErrConflict) {
		return r.ensureSecret(ctx, keyPair)
	}
	if err != nil {
		return registry.Secret{}, "", "", false, fmt.Errorf("create Secret %q: %w", keyPair.Metadata.Name, err)
	}
	secretResource, err := registry.SecretResource.Decode(created)
	if err != nil {
		return registry.Secret{}, "", "", false, fmt.Errorf("decode created Secret %q: %w", keyPair.Metadata.Name, err)
	}
	return secretResource, publicKey, fingerprint, true, nil
}

func validateOwnedSecret(secretResource registry.Secret, keyPair registry.SSHKeyPair) (string, string, error) {
	if secretResource.Metadata.Annotations[ownerUIDAnnotation] != keyPair.Metadata.UID {
		return "", "", fmt.Errorf("%w: Secret %q is not owned by SSHKeyPair %q", errSecretOwnershipConflict, secretResource.Metadata.Name, keyPair.Metadata.Name)
	}
	if secretResource.Metadata.DeletionTimestamp != nil {
		return "", "", fmt.Errorf("Secret %q is terminating", secretResource.Metadata.Name)
	}
	data := secretResource.Spec.Data
	if secretResource.Spec.SecretStoreRef.Name != keyPair.Spec.SecretStoreRef.Name || secretResource.Spec.Path != keyPair.Spec.Path || data["sshKeyPairUID"] != keyPair.Metadata.UID {
		return "", "", fmt.Errorf("%w: Secret %q has unexpected destination or ownership data", errSecretOwnershipConflict, secretResource.Metadata.Name)
	}
	publicKey := data["publicKey"]
	parsedPublic, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return "", "", fmt.Errorf("parse public key in Secret %q: %w", secretResource.Metadata.Name, err)
	}
	privateKey := data["privateKey"]
	parsedPrivate, err := ssh.ParseRawPrivateKey([]byte(privateKey))
	if err != nil {
		return "", "", fmt.Errorf("parse private key in Secret %q: %w", secretResource.Metadata.Name, err)
	}
	signer, err := ssh.NewSignerFromKey(parsedPrivate)
	if err != nil {
		return "", "", fmt.Errorf("derive public key from Secret %q private key: %w", secretResource.Metadata.Name, err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), parsedPublic.Marshal()) {
		return "", "", fmt.Errorf("public and private keys in Secret %q do not match", secretResource.Metadata.Name)
	}
	return publicKey, ssh.FingerprintSHA256(parsedPublic), nil
}

func secretReady(secretResource registry.Secret) bool {
	return secretResource.Status != nil && secretResource.Status.Phase != nil &&
		*secretResource.Status.Phase == apigen.SecretStatusPhaseReady && secretResource.Status.ObservedGeneration != nil &&
		*secretResource.Status.ObservedGeneration == secretResource.Metadata.Generation
}

func failureStatus(keyPair registry.SSHKeyPair, phase, reason, message string) *apigen.SSHKeyPairStatus {
	statusPhase, observedGeneration := apigen.SSHKeyPairStatusPhase(phase), keyPair.Metadata.Generation
	conditions := []apigen.SSHKeyPairCondition{{
		Type: "Ready", Status: apigen.SSHKeyPairConditionStatusFalse, Reason: reason,
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	return &apigen.SSHKeyPairStatus{
		Phase: &statusPhase, ObservedGeneration: &observedGeneration, Conditions: &conditions,
	}
}

func (r *Reconciler) updateStatus(ctx context.Context, keyPair registry.SSHKeyPair, status *apigen.SSHKeyPairStatus) error {
	if resource.EqualJSON(keyPair.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(keyPair.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse SSHKeyPair %q resource version: %w", keyPair.Metadata.Name, err)
	}
	storedStatus, err := registry.SSHKeyPairResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	if _, err := r.store.UpdateStatus(ctx, keyPair.Kind, keyPair.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update SSHKeyPair %q status: %w", keyPair.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) finalize(ctx context.Context, keyPair registry.SSHKeyPair) error {
	if !slices.Contains(keyPair.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	raw, err := r.store.Get(ctx, registry.SecretResource.Kind, keyPair.Metadata.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.removeFinalizer(ctx, keyPair)
	}
	if err != nil {
		return fmt.Errorf("get Secret %q while finalizing SSHKeyPair: %w", keyPair.Metadata.Name, err)
	}
	secretResource, err := registry.SecretResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode Secret %q while finalizing SSHKeyPair: %w", keyPair.Metadata.Name, err)
	}
	if secretResource.Metadata.Annotations[ownerUIDAnnotation] != keyPair.Metadata.UID {
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
		return fmt.Errorf("delete Secret %q while finalizing SSHKeyPair: %w", secretResource.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, keyPair registry.SSHKeyPair) error {
	keyPair.Metadata.Finalizers = slices.DeleteFunc(keyPair.Metadata.Finalizers, func(value string) bool { return value == cleanupFinalizer })
	encoded, err := keyPair.Encode()
	if err != nil {
		return fmt.Errorf("encode SSHKeyPair %q while removing finalizer: %w", keyPair.Metadata.Name, err)
	}
	revision, err := strconv.ParseInt(keyPair.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse SSHKeyPair %q resource version: %w", keyPair.Metadata.Name, err)
	}
	if _, err := r.store.Update(ctx, encoded, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove finalizer from SSHKeyPair %q: %w", keyPair.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) RequestsForSecret(_ context.Context, request controller.Request) ([]controller.Request, error) {
	return []controller.Request{{Kind: registry.SSHKeyPairResource.Kind, Name: request.Name}}, nil
}

func (r *Reconciler) RequestsForSSHKeyPair(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	servers, err := r.store.List(ctx, registry.ServerResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list Servers for SSHKeyPair %q: %w", request.Name, err)
	}
	requests := make([]controller.Request, 0)
	for _, raw := range servers.Items {
		server, err := registry.ServerResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode Server %q for SSHKeyPair %q: %w", raw.Metadata.Name, request.Name, err)
		}
		if serverReferencesKeyPair(server, request.Name) {
			requests = append(requests, controller.Request{Kind: registry.ServerResource.Kind, Name: server.Metadata.Name})
		}
	}
	return requests, nil
}

func serverReferencesKeyPair(server registry.Server, name string) bool {
	if server.Spec.Users == nil {
		return false
	}
	for _, user := range *server.Spec.Users {
		if user.Ssh == nil {
			continue
		}
		for _, reference := range user.Ssh.AuthorizedKeyRefs {
			if reference.Name == name {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) reconcileServerKeys(ctx context.Context, server registry.Server) error {
	type resolvedKey struct{ keyPairName, keyPairUID, loginUser, publicKey, fingerprint string }
	keys := make([]resolvedKey, 0)
	if server.Spec.Users != nil {
		for _, user := range *server.Spec.Users {
			if user.Ssh == nil {
				continue
			}
			for _, reference := range user.Ssh.AuthorizedKeyRefs {
				raw, err := r.store.Get(ctx, registry.SSHKeyPairResource.Kind, reference.Name)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return fmt.Errorf("get SSHKeyPair %q for Server %q: %w", reference.Name, server.Metadata.Name, err)
				}
				keyPair, err := registry.SSHKeyPairResource.Decode(raw)
				if err != nil {
					return fmt.Errorf("decode SSHKeyPair %q for Server %q: %w", reference.Name, server.Metadata.Name, err)
				}
				if keyPair.Metadata.DeletionTimestamp != nil || keyPair.Status == nil || keyPair.Status.Phase == nil ||
					*keyPair.Status.Phase != apigen.SSHKeyPairStatusPhaseReady || keyPair.Status.ObservedGeneration == nil ||
					*keyPair.Status.ObservedGeneration != keyPair.Metadata.Generation {
					continue
				}
				if keyPair.Status.PublicKey == nil || keyPair.Status.Fingerprint == nil || *keyPair.Status.PublicKey == "" || *keyPair.Status.Fingerprint == "" {
					continue
				}
				keys = append(keys, resolvedKey{keyPair.Metadata.Name, keyPair.Metadata.UID, user.Name, *keyPair.Status.PublicKey, *keyPair.Status.Fingerprint})
			}
		}
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].loginUser != keys[right].loginUser {
			return keys[left].loginUser < keys[right].loginUser
		}
		return keys[left].keyPairName < keys[right].keyPairName
	})
	authorizedKeys := make([]apigen.ServerSSHAuthorizedKeyStatus, 0, len(keys))
	for _, key := range keys {
		authorizedKeys = append(authorizedKeys, apigen.ServerSSHAuthorizedKeyStatus{
			KeyPairRef: apigen.ResourceReference{Name: key.keyPairName, Uid: key.keyPairUID},
			LoginUser:  key.loginUser, PublicKey: key.publicKey, Fingerprint: key.fingerprint,
		})
	}
	status := &apigen.ServerStatus{}
	if server.Status != nil {
		*status = *server.Status
	}
	status.Ssh = &apigen.ServerSSHStatus{AuthorizedKeys: authorizedKeys}
	if resource.EqualJSON(server.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(server.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse Server %q resource version: %w", server.Metadata.Name, err)
	}
	storedStatus, err := registry.ServerResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	if _, err := r.store.UpdateStatus(ctx, server.Kind, server.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update Server %q resolved SSH keys: %w", server.Metadata.Name, err)
	}
	return nil
}
