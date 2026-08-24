package sshaccess

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

const cleanupFinalizer = "homelab.io/ssh-access-cleanup"

type Reconciler struct {
	store    store.Store
	keyStore KeyStore
}

func NewReconciler(store store.Store) *Reconciler {
	return &Reconciler{store: store, keyStore: NewOpenBaoKeyStore()}
}

func NewReconcilerWithKeyStore(store store.Store, keyStore KeyStore) *Reconciler {
	return &Reconciler{store: store, keyStore: keyStore}
}

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
		revision, err := strconv.ParseInt(grant.Metadata.ResourceVersion, 10, 64)
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
	provider := secretStore.Spec.Provider.OpenBao
	prefix := ""
	if provider.KeyPrefix != nil {
		prefix = *provider.KeyPrefix
	}
	keyName := grant.Metadata.Name
	if configured := grant.Spec.Credential.GeneratedKeyPair.KeyName; configured != nil {
		keyName = *configured
	}
	logicalPath, err := logicalKeyPath(prefix, server.Metadata.Name, keyName)
	if err != nil {
		if updateErr := r.updateGrantFailure(ctx, grant, "Failed", "InvalidSecretPath", err.Error()); updateErr != nil {
			return updateErr
		}
		return r.reconcileServerKeys(ctx, server)
	}
	pair, err := r.keyStore.EnsureKeyPair(ctx, provider, logicalPath, KeyOwnership{
		ServerUID: server.Metadata.UID,
		GrantUID:  grant.Metadata.UID,
	})
	if err != nil {
		phase, reason := "Failed", "KeyPairProvisioningFailed"
		if containsOwnershipConflict(err) {
			phase, reason = "Conflict", "SecretOwnershipConflict"
		}
		if updateErr := r.updateGrantFailure(ctx, grant, phase, reason, err.Error()); updateErr != nil {
			return updateErr
		}
		return r.reconcileServerKeys(ctx, server)
	}
	if err := r.updateGrantSuccess(ctx, grant, server, secretStore, logicalPath, pair); err != nil {
		return err
	}
	return r.reconcileServerKeys(ctx, server)
}

func (r *Reconciler) finalizeGrant(ctx context.Context, grant registry.SSHAccessGrant) error {
	if !hasFinalizer(grant.Metadata.Finalizers, cleanupFinalizer) {
		return r.reconcileAllServerKeys(ctx)
	}

	serverUID := statusReferenceUID(grant.Status, "serverRef")
	serverRaw, serverErr := r.store.Get(ctx, registry.ServerResource.Kind, grant.Spec.ServerRef.Name)
	if serverErr == nil {
		serverUID = serverRaw.Metadata.UID
	} else if !errors.Is(serverErr, store.ErrNotFound) {
		return fmt.Errorf("get Server %q while finalizing SSHAccessGrant %q: %w", grant.Spec.ServerRef.Name, grant.Metadata.Name, serverErr)
	}

	storeName := grant.Spec.Credential.GeneratedKeyPair.SecretStoreRef.Name
	secretStoreRaw, err := r.store.Get(ctx, registry.SecretStoreResource.Kind, storeName)
	if errors.Is(err, store.ErrNotFound) && grant.Status["secret"] == nil {
		return r.finishGrantDeletion(ctx, grant)
	}
	if err != nil {
		return fmt.Errorf("get SecretStore %q while finalizing SSHAccessGrant %q: %w", storeName, grant.Metadata.Name, err)
	}
	secretStore, err := registry.SecretStoreResource.Decode(secretStoreRaw)
	if err != nil {
		return fmt.Errorf("decode SecretStore %q while finalizing SSHAccessGrant %q: %w", storeName, grant.Metadata.Name, err)
	}

	logicalPath := statusLogicalPath(grant.Status)
	if logicalPath == "" {
		prefix := ""
		if secretStore.Spec.Provider.OpenBao.KeyPrefix != nil {
			prefix = *secretStore.Spec.Provider.OpenBao.KeyPrefix
		}
		keyName := grant.Metadata.Name
		if configured := grant.Spec.Credential.GeneratedKeyPair.KeyName; configured != nil {
			keyName = *configured
		}
		logicalPath, err = logicalKeyPath(prefix, grant.Spec.ServerRef.Name, keyName)
		if err != nil {
			return fmt.Errorf("derive secret path while finalizing SSHAccessGrant %q: %w", grant.Metadata.Name, err)
		}
	}
	if serverErr == nil {
		server, err := registry.ServerResource.Decode(serverRaw)
		if err != nil {
			return fmt.Errorf("decode Server %q while finalizing SSHAccessGrant %q: %w", grant.Spec.ServerRef.Name, grant.Metadata.Name, err)
		}
		if err := r.reconcileServerKeys(ctx, server); err != nil {
			return err
		}
	}
	if serverUID != "" {
		if err := r.keyStore.DeleteKeyPair(ctx, secretStore.Spec.Provider.OpenBao, logicalPath, KeyOwnership{
			ServerUID: serverUID,
			GrantUID:  grant.Metadata.UID,
		}); err != nil {
			return fmt.Errorf("delete key pair while finalizing SSHAccessGrant %q: %w", grant.Metadata.Name, err)
		}
	}
	return r.finishGrantDeletion(ctx, grant)
}

func (r *Reconciler) finishGrantDeletion(ctx context.Context, grant registry.SSHAccessGrant) error {
	grant.Metadata.Finalizers = removeFinalizer(grant.Metadata.Finalizers, cleanupFinalizer)
	encoded, err := grant.Encode()
	if err != nil {
		return fmt.Errorf("encode SSHAccessGrant %q while removing finalizer: %w", grant.Metadata.Name, err)
	}
	revision, err := strconv.ParseInt(grant.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse SSHAccessGrant %q resource version: %w", grant.Metadata.Name, err)
	}
	if _, err := r.store.Update(ctx, encoded, revision); err != nil {
		return fmt.Errorf("remove finalizer from SSHAccessGrant %q: %w", grant.Metadata.Name, err)
	}
	return nil
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

func statusReferenceUID(status map[string]any, field string) string {
	reference, _ := status[field].(map[string]any)
	uid, _ := reference["uid"].(string)
	return uid
}

func statusLogicalPath(status map[string]any) string {
	secret, _ := status["secret"].(map[string]any)
	logicalPath, _ := secret["logicalPath"].(string)
	return logicalPath
}

func containsOwnershipConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ownership conflict")
}

func (r *Reconciler) RequestsForServer(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(grant registry.SSHAccessGrant) bool {
		return grant.Spec.ServerRef.Name == request.Name
	})
}

func (r *Reconciler) RequestsForSecretStore(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(grant registry.SSHAccessGrant) bool {
		return grant.Spec.Credential.GeneratedKeyPair.SecretStoreRef.Name == request.Name
	})
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

func (r *Reconciler) updateGrantSuccess(ctx context.Context, grant registry.SSHAccessGrant, server registry.Server, secretStore registry.SecretStore, logicalPath string, pair KeyPair) error {
	status := map[string]any{
		"phase":              "Ready",
		"observedGeneration": grant.Metadata.Generation,
		"serverRef":          map[string]any{"name": server.Metadata.Name, "uid": server.Metadata.UID},
		"publicKey":          pair.PublicKey,
		"fingerprint":        pair.Fingerprint,
		"secret": map[string]any{
			"storeRef":    map[string]any{"name": secretStore.Metadata.Name, "uid": secretStore.Metadata.UID},
			"logicalPath": logicalPath,
			"version":     pair.Version,
		},
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "KeyPairAvailable",
			"message": "The generated key pair is stored in OpenBao", "observedGeneration": grant.Metadata.Generation,
		}},
	}
	return r.writeGrantStatus(ctx, grant, status)
}

func (r *Reconciler) updateGrantFailure(ctx context.Context, grant registry.SSHAccessGrant, phase, reason, message string) error {
	status := map[string]any{
		"phase":              phase,
		"observedGeneration": grant.Metadata.Generation,
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": reason,
			"message": message, "observedGeneration": grant.Metadata.Generation,
		}},
	}
	return r.writeGrantStatus(ctx, grant, status)
}

func (r *Reconciler) writeGrantStatus(ctx context.Context, grant registry.SSHAccessGrant, status map[string]any) error {
	if resource.EqualJSON(grant.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(grant.Metadata.ResourceVersion, 10, 64)
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
	type resolvedKey struct {
		grantName   string
		loginUser   string
		publicKey   string
		fingerprint string
		grantUID    string
	}
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
		keys = append(keys, resolvedKey{
			grantName: grant.Metadata.Name, grantUID: grant.Metadata.UID,
			loginUser: grant.Spec.LoginUser, publicKey: publicKey, fingerprint: fingerprint,
		})
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].loginUser != keys[right].loginUser {
			return keys[left].loginUser < keys[right].loginUser
		}
		return keys[left].grantName < keys[right].grantName
	})
	authorizedKeys := make([]any, 0, len(keys))
	for _, key := range keys {
		authorizedKeys = append(authorizedKeys, map[string]any{
			"accessGrantRef": map[string]any{"name": key.grantName, "uid": key.grantUID},
			"loginUser":      key.loginUser, "publicKey": key.publicKey, "fingerprint": key.fingerprint,
		})
	}
	status := cloneStatus(server.Status)
	status["ssh"] = map[string]any{"authorizedKeys": authorizedKeys}
	if resource.EqualJSON(server.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(server.Metadata.ResourceVersion, 10, 64)
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
