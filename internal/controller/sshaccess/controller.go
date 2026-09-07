package sshaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

const (
	cleanupFinalizer       = "homelab.io/ssh-access-cleanup"
	secretCleanupFinalizer = "homelab.io/secret-cleanup"
	grantUIDAnnotation     = "homelab.io/ssh-access-grant-uid"
	serverUIDAnnotation    = "homelab.io/server-uid"
)

type Reconciler struct{ store store.Store }

func NewReconciler(store store.Store) *Reconciler { return &Reconciler{store: store} }

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.SSHAccessGrantResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.reconcileAllServerKeys(ctx)
	}
	if err != nil {
		return fmt.Errorf("get SSHAccessGrant %q: %w", request.Name, err)
	}
	grant, err := registry.SSHAccessGrantResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode SSHAccessGrant %q: %w", request.Name, err)
	}
	if grant.Metadata.DeletionTimestamp != nil {
		return r.finalizeGrant(ctx, grant)
	}
	if !hasFinalizer(grant.Metadata.Finalizers, cleanupFinalizer) {
		grant.Metadata.Finalizers = append(grant.Metadata.Finalizers, cleanupFinalizer)
		encoded, err := grant.Encode()
		if err != nil {
			return fmt.Errorf("encode SSHAccessGrant %q while adding finalizer: %w", grant.Metadata.Name, err)
		}
		revision, err := resourceRevision(grant.Metadata.ResourceVersion)
		if err != nil {
			return fmt.Errorf("parse SSHAccessGrant %q resource version: %w", grant.Metadata.Name, err)
		}
		if _, err := r.store.Update(ctx, encoded, revision); err != nil {
			return fmt.Errorf("add finalizer to SSHAccessGrant %q: %w", grant.Metadata.Name, err)
		}
		return nil
	}

	serverRaw, err := r.store.Get(ctx, registry.ServerResource.Kind, grant.Spec.ServerRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		if err := r.updateGrantFailure(ctx, grant, "Pending", "ServerNotFound", fmt.Sprintf("Server %q does not exist", grant.Spec.ServerRef.Name)); err != nil {
			return err
		}
		return r.reconcileAllServerKeys(ctx)
	}
	if err != nil {
		return fmt.Errorf("get Server %q: %w", grant.Spec.ServerRef.Name, err)
	}
	server, err := registry.ServerResource.Decode(serverRaw)
	if err != nil {
		return fmt.Errorf("decode Server %q: %w", grant.Spec.ServerRef.Name, err)
	}

	storeName := grant.Spec.Credential.GeneratedKeyPair.SecretStoreRef.Name
	secretStoreRaw, err := r.store.Get(ctx, registry.SecretStoreResource.Kind, storeName)
	if errors.Is(err, store.ErrNotFound) {
		if err := r.updateGrantFailure(ctx, grant, "Pending", "SecretStoreNotFound", fmt.Sprintf("SecretStore %q does not exist", storeName)); err != nil {
			return err
		}
		return r.reconcileServerKeys(ctx, server)
	}
	if err != nil {
		return fmt.Errorf("get SecretStore %q: %w", storeName, err)
	}
	secretStore, err := registry.SecretStoreResource.Decode(secretStoreRaw)
	if err != nil {
		return fmt.Errorf("decode SecretStore %q: %w", storeName, err)
	}
	secretPath, logicalPath, err := keyPaths(secretStore, server.Metadata.Name, keyName(grant))
	if err != nil {
		if updateErr := r.updateGrantFailure(ctx, grant, "Failed", "InvalidSecretPath", err.Error()); updateErr != nil {
			return updateErr
		}
		return r.reconcileServerKeys(ctx, server)
	}
	secretResource, pair, created, err := r.ensureSecret(ctx, grant, server, secretStore, secretPath)
	if err != nil {
		phase, reason := "Failed", "SecretProvisioningFailed"
		if errors.Is(err, errSecretOwnershipConflict) {
			phase, reason = "Conflict", "SecretOwnershipConflict"
		}
		if updateErr := r.updateGrantFailure(ctx, grant, phase, reason, err.Error()); updateErr != nil {
			return updateErr
		}
		return r.reconcileServerKeys(ctx, server)
	}
	if created || !secretReady(secretResource) {
		if phase, _ := secretResource.Status["phase"].(string); phase == "Failed" {
			return r.updateGrantFailure(ctx, grant, "Failed", "SecretReconciliationFailed", fmt.Sprintf("Secret %q failed reconciliation", secretResource.Metadata.Name))
		}
		if err := r.updateGrantFailure(ctx, grant, "Pending", "SecretPending", fmt.Sprintf("Secret %q is waiting for reconciliation", secretResource.Metadata.Name)); err != nil {
			return err
		}
		return r.reconcileServerKeys(ctx, server)
	}
	version, _ := numericInt64(secretResource.Status["externalVersion"])
	if version < 1 {
		version = 1
	}
	if err := r.updateGrantSuccess(ctx, grant, server, secretStore, logicalPath, pair, version); err != nil {
		return err
	}
	return r.reconcileServerKeys(ctx, server)
}

var errSecretOwnershipConflict = errors.New("Secret ownership conflict")

type keyPair struct{ privateKey, publicKey, fingerprint string }

func (r *Reconciler) ensureSecret(ctx context.Context, grant registry.SSHAccessGrant, server registry.Server, secretStore registry.SecretStore, secretPath string) (registry.Secret, keyPair, bool, error) {
	raw, err := r.store.Get(ctx, registry.SecretResource.Kind, grant.Metadata.Name)
	if err == nil {
		secretResource, decodeErr := registry.SecretResource.Decode(raw)
		if decodeErr != nil {
			return registry.Secret{}, keyPair{}, false, fmt.Errorf("decode Secret %q: %w", grant.Metadata.Name, decodeErr)
		}
		pair, validateErr := validateOwnedSecret(secretResource, grant, server, secretStore, secretPath)
		return secretResource, pair, false, validateErr
	}
	if !errors.Is(err, store.ErrNotFound) {
		return registry.Secret{}, keyPair{}, false, fmt.Errorf("get Secret %q: %w", grant.Metadata.Name, err)
	}
	privateKey, publicKey, fingerprint, err := generateEd25519KeyPair()
	if err != nil {
		return registry.Secret{}, keyPair{}, false, err
	}
	desired := registry.NewSecret(resource.Metadata{
		Name: grant.Metadata.Name, Finalizers: []string{secretCleanupFinalizer},
		Annotations: map[string]string{grantUIDAnnotation: grant.Metadata.UID, serverUIDAnnotation: server.Metadata.UID},
	}, apigen.SecretSpec{
		SecretStoreRef: apigen.SecretStoreReference{Name: secretStore.Metadata.Name}, Path: secretPath,
		Data: map[string]string{"privateKey": privateKey, "publicKey": publicKey, "serverUID": server.Metadata.UID, "accessGrantUID": grant.Metadata.UID},
	})
	encoded, err := desired.Encode()
	if err != nil {
		return registry.Secret{}, keyPair{}, false, fmt.Errorf("encode Secret %q: %w", grant.Metadata.Name, err)
	}
	created, err := r.store.Create(ctx, encoded)
	if errors.Is(err, store.ErrConflict) {
		existing, getErr := r.store.Get(ctx, registry.SecretResource.Kind, grant.Metadata.Name)
		if getErr != nil {
			return registry.Secret{}, keyPair{}, false, fmt.Errorf("get Secret %q after create conflict: %w", grant.Metadata.Name, getErr)
		}
		secretResource, decodeErr := registry.SecretResource.Decode(existing)
		if decodeErr != nil {
			return registry.Secret{}, keyPair{}, false, fmt.Errorf("decode Secret %q after create conflict: %w", grant.Metadata.Name, decodeErr)
		}
		pair, validateErr := validateOwnedSecret(secretResource, grant, server, secretStore, secretPath)
		return secretResource, pair, false, validateErr
	}
	if err != nil {
		return registry.Secret{}, keyPair{}, false, fmt.Errorf("create Secret %q: %w", grant.Metadata.Name, err)
	}
	secretResource, err := registry.SecretResource.Decode(created)
	if err != nil {
		return registry.Secret{}, keyPair{}, false, fmt.Errorf("decode created Secret %q: %w", grant.Metadata.Name, err)
	}
	return secretResource, keyPair{privateKey, publicKey, fingerprint}, true, nil
}

func validateOwnedSecret(secretResource registry.Secret, grant registry.SSHAccessGrant, server registry.Server, secretStore registry.SecretStore, secretPath string) (keyPair, error) {
	annotations := secretResource.Metadata.Annotations
	if annotations[grantUIDAnnotation] != grant.Metadata.UID || annotations[serverUIDAnnotation] != server.Metadata.UID {
		return keyPair{}, fmt.Errorf("%w: Secret %q is not owned by SSHAccessGrant %q", errSecretOwnershipConflict, secretResource.Metadata.Name, grant.Metadata.Name)
	}
	if secretResource.Metadata.DeletionTimestamp != nil {
		return keyPair{}, fmt.Errorf("Secret %q is terminating", secretResource.Metadata.Name)
	}
	if secretResource.Spec.SecretStoreRef.Name != secretStore.Metadata.Name || secretResource.Spec.Path != secretPath || secretResource.Spec.Data["serverUID"] != server.Metadata.UID || secretResource.Spec.Data["accessGrantUID"] != grant.Metadata.UID {
		return keyPair{}, fmt.Errorf("%w: Secret %q has unexpected destination or ownership data", errSecretOwnershipConflict, secretResource.Metadata.Name)
	}
	publicKey := strings.TrimSpace(secretResource.Spec.Data["publicKey"])
	parsedPublic, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return keyPair{}, fmt.Errorf("parse public key in Secret %q: %w", secretResource.Metadata.Name, err)
	}
	privateKey := secretResource.Spec.Data["privateKey"]
	parsedPrivate, err := ssh.ParseRawPrivateKey([]byte(privateKey))
	if err != nil {
		return keyPair{}, fmt.Errorf("parse private key in Secret %q: %w", secretResource.Metadata.Name, err)
	}
	signer, err := ssh.NewSignerFromKey(parsedPrivate)
	if err != nil {
		return keyPair{}, fmt.Errorf("derive public key from Secret %q private key: %w", secretResource.Metadata.Name, err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), parsedPublic.Marshal()) {
		return keyPair{}, fmt.Errorf("public and private keys in Secret %q do not match", secretResource.Metadata.Name)
	}
	return keyPair{privateKey, publicKey, ssh.FingerprintSHA256(parsedPublic)}, nil
}

func secretReady(secretResource registry.Secret) bool {
	observed, ok := numericInt64(secretResource.Status["observedGeneration"])
	return secretResource.Status["phase"] == "Ready" && ok && observed == secretResource.Metadata.Generation
}

func numericInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	default:
		return 0, false
	}
}

func (r *Reconciler) finalizeGrant(ctx context.Context, grant registry.SSHAccessGrant) error {
	if !hasFinalizer(grant.Metadata.Finalizers, cleanupFinalizer) {
		return r.reconcileAllServerKeys(ctx)
	}
	if serverRaw, err := r.store.Get(ctx, registry.ServerResource.Kind, grant.Spec.ServerRef.Name); err == nil {
		server, decodeErr := registry.ServerResource.Decode(serverRaw)
		if decodeErr != nil {
			return fmt.Errorf("decode Server %q while finalizing SSHAccessGrant %q: %w", grant.Spec.ServerRef.Name, grant.Metadata.Name, decodeErr)
		}
		if err := r.reconcileServerKeys(ctx, server); err != nil {
			return err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get Server %q while finalizing SSHAccessGrant %q: %w", grant.Spec.ServerRef.Name, grant.Metadata.Name, err)
	}
	secretRaw, err := r.store.Get(ctx, registry.SecretResource.Kind, grant.Metadata.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.finishGrantDeletion(ctx, grant)
	}
	if err != nil {
		return fmt.Errorf("get Secret %q while finalizing SSHAccessGrant: %w", grant.Metadata.Name, err)
	}
	secretResource, err := registry.SecretResource.Decode(secretRaw)
	if err != nil {
		return fmt.Errorf("decode Secret %q while finalizing SSHAccessGrant: %w", grant.Metadata.Name, err)
	}
	if secretResource.Metadata.Annotations[grantUIDAnnotation] != grant.Metadata.UID {
		return fmt.Errorf("%w: refusing to delete Secret %q", errSecretOwnershipConflict, secretResource.Metadata.Name)
	}
	if secretResource.Metadata.DeletionTimestamp != nil {
		return nil
	}
	revision, err := resourceRevision(secretResource.Metadata.ResourceVersion)
	if err != nil {
		return fmt.Errorf("parse Secret %q resource version: %w", secretResource.Metadata.Name, err)
	}
	if err := r.store.Delete(ctx, secretResource.Kind, secretResource.Metadata.Name, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("delete Secret %q while finalizing SSHAccessGrant: %w", secretResource.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) finishGrantDeletion(ctx context.Context, grant registry.SSHAccessGrant) error {
	grant.Metadata.Finalizers = removeFinalizer(grant.Metadata.Finalizers, cleanupFinalizer)
	encoded, err := grant.Encode()
	if err != nil {
		return fmt.Errorf("encode SSHAccessGrant %q while removing finalizer: %w", grant.Metadata.Name, err)
	}
	revision, err := resourceRevision(grant.Metadata.ResourceVersion)
	if err != nil {
		return fmt.Errorf("parse SSHAccessGrant %q resource version: %w", grant.Metadata.Name, err)
	}
	if _, err := r.store.Update(ctx, encoded, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove finalizer from SSHAccessGrant %q: %w", grant.Metadata.Name, err)
	}
	return nil
}

func keyName(grant registry.SSHAccessGrant) string {
	if configured := grant.Spec.Credential.GeneratedKeyPair.KeyName; configured != nil {
		return *configured
	}
	return grant.Metadata.Name
}

func keyPaths(secretStore registry.SecretStore, serverName, keyName string) (string, string, error) {
	relative, err := logicalKeyPath("", serverName, keyName)
	if err != nil {
		return "", "", err
	}
	prefix := ""
	if provider := secretStore.Spec.Provider.OpenBao; provider != nil && provider.KeyPrefix != nil {
		prefix = *provider.KeyPrefix
	}
	logical, err := logicalKeyPath(prefix, serverName, keyName)
	return relative, logical, err
}

func logicalKeyPath(prefix, serverName, keyName string) (string, error) {
	segments := []string{}
	if prefix != "" {
		segments = append(segments, strings.Split(strings.Trim(prefix, "/"), "/")...)
	}
	segments = append(segments, serverName, "ssh-keys", keyName)
	joined := strings.Join(segments, "/")
	if joined == "" || path.Clean(joined) != joined || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf("secret key prefix %q is not canonical", prefix)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("secret key path %q is not canonical", joined)
		}
	}
	return joined, nil
}

func hasFinalizer(finalizers []string, expected string) bool {
	for _, finalizer := range finalizers {
		if finalizer == expected {
			return true
		}
	}
	return false
}

func removeFinalizer(finalizers []string, removed string) []string {
	result := make([]string, 0, len(finalizers))
	for _, finalizer := range finalizers {
		if finalizer != removed {
			result = append(result, finalizer)
		}
	}
	return result
}

func resourceRevision(version string) (int64, error) { return strconv.ParseInt(version, 10, 64) }

func (r *Reconciler) RequestsForServer(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(grant registry.SSHAccessGrant) bool { return grant.Spec.ServerRef.Name == request.Name })
}

func (r *Reconciler) RequestsForSecretStore(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(grant registry.SSHAccessGrant) bool {
		return grant.Spec.Credential.GeneratedKeyPair.SecretStoreRef.Name == request.Name
	})
}

func (r *Reconciler) RequestsForSecret(_ context.Context, request controller.Request) ([]controller.Request, error) {
	return []controller.Request{{Kind: registry.SSHAccessGrantResource.Kind, Name: request.Name}}, nil
}

func (r *Reconciler) requestsMatching(ctx context.Context, matches func(registry.SSHAccessGrant) bool) ([]controller.Request, error) {
	list, err := r.store.List(ctx, registry.SSHAccessGrantResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list SSHAccessGrants: %w", err)
	}
	requests := make([]controller.Request, 0, len(list.Items))
	for _, raw := range list.Items {
		grant, err := registry.SSHAccessGrantResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode SSHAccessGrant %q: %w", raw.Metadata.Name, err)
		}
		if matches(grant) {
			requests = append(requests, controller.Request{Kind: grant.Kind, Name: grant.Metadata.Name})
		}
	}
	return requests, nil
}

func (r *Reconciler) updateGrantSuccess(ctx context.Context, grant registry.SSHAccessGrant, server registry.Server, secretStore registry.SecretStore, logicalPath string, pair keyPair, version int64) error {
	status := map[string]any{
		"phase": "Ready", "observedGeneration": grant.Metadata.Generation,
		"serverRef": map[string]any{"name": server.Metadata.Name, "uid": server.Metadata.UID},
		"publicKey": pair.publicKey, "fingerprint": pair.fingerprint,
		"secret":     map[string]any{"storeRef": map[string]any{"name": secretStore.Metadata.Name, "uid": secretStore.Metadata.UID}, "logicalPath": logicalPath, "version": version},
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "KeyPairAvailable", "message": "The generated key pair is managed by a Ready Secret resource", "observedGeneration": grant.Metadata.Generation}},
	}
	return r.writeGrantStatus(ctx, grant, status)
}

func (r *Reconciler) updateGrantFailure(ctx context.Context, grant registry.SSHAccessGrant, phase, reason, message string) error {
	status := map[string]any{
		"phase": phase, "observedGeneration": grant.Metadata.Generation,
		"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": reason, "message": message, "observedGeneration": grant.Metadata.Generation}},
	}
	return r.writeGrantStatus(ctx, grant, status)
}

func (r *Reconciler) writeGrantStatus(ctx context.Context, grant registry.SSHAccessGrant, status map[string]any) error {
	if resource.EqualJSON(grant.Status, status) {
		return nil
	}
	revision, err := resourceRevision(grant.Metadata.ResourceVersion)
	if err != nil {
		return fmt.Errorf("parse SSHAccessGrant %q resource version: %w", grant.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, grant.Kind, grant.Metadata.Name, status, revision); err != nil {
		return fmt.Errorf("update SSHAccessGrant %q status: %w", grant.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) reconcileAllServerKeys(ctx context.Context) error {
	servers, err := r.store.List(ctx, registry.ServerResource.Kind)
	if err != nil {
		return fmt.Errorf("list Servers while resolving SSH keys: %w", err)
	}
	for _, raw := range servers.Items {
		server, err := registry.ServerResource.Decode(raw)
		if err != nil {
			return fmt.Errorf("decode Server %q: %w", raw.Metadata.Name, err)
		}
		if err := r.reconcileServerKeys(ctx, server); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileServerKeys(ctx context.Context, server registry.Server) error {
	list, err := r.store.List(ctx, registry.SSHAccessGrantResource.Kind)
	if err != nil {
		return fmt.Errorf("list SSHAccessGrants for Server %q: %w", server.Metadata.Name, err)
	}
	type resolvedKey struct{ grantName, loginUser, publicKey, fingerprint, grantUID string }
	keys := make([]resolvedKey, 0)
	for _, raw := range list.Items {
		grant, err := registry.SSHAccessGrantResource.Decode(raw)
		if err != nil {
			return fmt.Errorf("decode SSHAccessGrant %q: %w", raw.Metadata.Name, err)
		}
		if grant.Metadata.DeletionTimestamp != nil || grant.Spec.ServerRef.Name != server.Metadata.Name || grant.Status["phase"] != "Ready" {
			continue
		}
		serverRef, ok := grant.Status["serverRef"].(map[string]any)
		if !ok || serverRef["uid"] != server.Metadata.UID {
			continue
		}
		publicKey, publicOK := grant.Status["publicKey"].(string)
		fingerprint, fingerprintOK := grant.Status["fingerprint"].(string)
		if !publicOK || !fingerprintOK || publicKey == "" || fingerprint == "" {
			continue
		}
		keys = append(keys, resolvedKey{grant.Metadata.Name, grant.Spec.LoginUser, publicKey, fingerprint, grant.Metadata.UID})
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].loginUser != keys[right].loginUser {
			return keys[left].loginUser < keys[right].loginUser
		}
		return keys[left].grantName < keys[right].grantName
	})
	authorizedKeys := make([]any, 0, len(keys))
	for _, key := range keys {
		authorizedKeys = append(authorizedKeys, map[string]any{"accessGrantRef": map[string]any{"name": key.grantName, "uid": key.grantUID}, "loginUser": key.loginUser, "publicKey": key.publicKey, "fingerprint": key.fingerprint})
	}
	status := cloneStatus(server.Status)
	status["ssh"] = map[string]any{"authorizedKeys": authorizedKeys}
	if resource.EqualJSON(server.Status, status) {
		return nil
	}
	revision, err := resourceRevision(server.Metadata.ResourceVersion)
	if err != nil {
		return fmt.Errorf("parse Server %q resource version: %w", server.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, server.Kind, server.Metadata.Name, status, revision); err != nil {
		return fmt.Errorf("update Server %q resolved SSH keys: %w", server.Metadata.Name, err)
	}
	return nil
}

func cloneStatus(status map[string]any) map[string]any {
	cloned := make(map[string]any, len(status)+1)
	for key, value := range status {
		cloned[key] = value
	}
	return cloned
}
