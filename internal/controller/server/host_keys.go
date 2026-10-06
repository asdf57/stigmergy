package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const hostKeyFinalizer = "homelab.io/server-host-key-cleanup"
const hostKeyOwner = "homelab.io/server-uid"

// HostKeyReconciler owns resource relationships only. Installation is external.
type HostKeyReconciler struct {
	store       store.Store
	secretStore string
}

func NewHostKeyReconciler(s store.Store, secretStore string) *HostKeyReconciler {
	return &HostKeyReconciler{store: s, secretStore: secretStore}
}

func (r *HostKeyReconciler) RequestsForDependency(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	values, err := r.store.List(ctx, "Server")
	if err != nil {
		return nil, err
	}
	result := make([]controller.Request, 0, len(values.Items))
	for _, value := range values.Items {
		result = append(result, controller.Request{Kind: "Server", Name: value.Metadata.Name})
	}
	return result, nil
}

func (r *HostKeyReconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, "Server", request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	server, err := registry.ServerResource.Decode(raw)
	if err != nil {
		return err
	}
	name := "server-host-" + server.Metadata.UID
	if server.Metadata.DeletionTimestamp != nil {
		return r.finalizeHostKey(ctx, server, name)
	}
	// Existing resources acquire the finalizer before child creation.
	if !slices.Contains(server.Metadata.Finalizers, hostKeyFinalizer) {
		raw.Metadata.Finalizers = append(raw.Metadata.Finalizers, hostKeyFinalizer)
		revision, e := strconv.ParseInt(raw.Metadata.ResourceVersion, 10, 64)
		if e != nil {
			return e
		}
		_, err = r.store.Update(ctx, raw, revision)
		return err
	}
	status := cloneServerStatus(server.Status)
	host := &apigen.ServerHostSSHStatus{}
	if status.HostSSH != nil {
		*host = *status.HostSSH
	}
	ready := false
	host.KeyReady = &ready
	fail := func(reason, message string) error {
		// Do not overwrite external observations. Only the public key projection is owned here.
		status.HostSSH = host
		status.Conditions = hostKeyCondition(status.Conditions, "False", reason, message, server.Metadata.Generation)
		return r.writeHostStatus(ctx, server, status)
	}
	keyRaw, err := r.store.Get(ctx, "SSHKeyPair", name)
	if errors.Is(err, store.ErrNotFound) {
		if host.KeyPairRef != nil {
			return fail("HostKeyMissing", "Established host key is missing; refusing identity replacement")
		}
		if _, e := r.store.Get(ctx, "SecretStore", r.secretStore); errors.Is(e, store.ErrNotFound) {
			return fail("SecretStoreMissing", "Host-key SecretStore is not configured")
		} else if e != nil {
			return e
		}
		key := registry.NewSSHKeyPair(resource.Metadata{Name: name, Annotations: map[string]string{hostKeyOwner: server.Metadata.UID}, Finalizers: append([]string(nil), registry.SSHKeyPairResource.DefaultFinalizers...)}, apigen.SSHKeyPairSpec{Algorithm: "ed25519", SecretStoreRef: apigen.SSHKeyPairSecretStoreReference{Name: r.secretStore}, Path: "ssh/hosts/" + server.Metadata.UID})
		encoded, e := key.Encode()
		if e != nil {
			return e
		}
		keyRaw, err = r.store.Create(ctx, encoded)
	}
	if err != nil {
		return err
	}
	key, err := registry.SSHKeyPairResource.Decode(keyRaw)
	if err != nil {
		return err
	}
	if key.Metadata.Annotations[hostKeyOwner] != server.Metadata.UID || (host.KeyPairRef != nil && host.KeyPairRef.Uid != key.Metadata.UID) {
		return fail("HostKeyOwnershipConflict", "Refusing to adopt a different host-key identity")
	}
	host.KeyPairRef = &apigen.ResourceReference{Name: key.Metadata.Name, Uid: key.Metadata.UID}
	if key.Metadata.DeletionTimestamp != nil || key.Status == nil || key.Status.Phase == nil || *key.Status.Phase != apigen.SSHKeyPairStatusPhaseReady || key.Status.ObservedGeneration == nil || *key.Status.ObservedGeneration != key.Metadata.Generation || key.Status.PublicKey == nil || key.Status.Fingerprint == nil {
		return fail("HostKeyPending", "Owned SSHKeyPair is not Ready")
	}
	if host.Fingerprint != nil && *host.Fingerprint != *key.Status.Fingerprint {
		return fail("HostKeyIdentityChanged", "Refusing an unapproved host-key rotation")
	}
	ready = true
	host.PublicKey, host.Fingerprint = key.Status.PublicKey, key.Status.Fingerprint
	status.HostSSH = host
	status.Conditions = hostKeyCondition(status.Conditions, "True", "HostKeyResolved", "Owned host-key material is Ready; installation is reported separately", server.Metadata.Generation)
	return r.writeHostStatus(ctx, server, status)
}

func (r *HostKeyReconciler) writeHostStatus(ctx context.Context, server registry.Server, status *apigen.ServerStatus) error {
	if status.Conditions != nil {
		sort.Slice(*status.Conditions, func(i, j int) bool { return (*status.Conditions)[i].Type < (*status.Conditions)[j].Type })
	}
	if resource.EqualJSON(server.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(server.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	encoded, err := registry.ServerResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, "Server", server.Metadata.Name, encoded, revision)
	return err
}

func hostKeyCondition(conditions *[]apigen.ServerCondition, state, reason, message string, generation int64) *[]apigen.ServerCondition {
	return upsertCondition(conditions, apigen.ServerCondition{Type: "HostKeyAvailable", Status: apigen.ServerConditionStatus(state), Reason: reason, Message: &message, ObservedGeneration: &generation})
}

func (r *HostKeyReconciler) finalizeHostKey(ctx context.Context, server registry.Server, name string) error {
	if !slices.Contains(server.Metadata.Finalizers, hostKeyFinalizer) {
		return nil
	}
	key, err := r.store.Get(ctx, "SSHKeyPair", name)
	if err == nil {
		if key.Metadata.Annotations[hostKeyOwner] != server.Metadata.UID {
			return fmt.Errorf("refusing to delete another owner's host key")
		}
		if key.Metadata.DeletionTimestamp != nil {
			return nil
		}
		revision, e := strconv.ParseInt(key.Metadata.ResourceVersion, 10, 64)
		if e != nil {
			return e
		}
		return r.store.Delete(ctx, "SSHKeyPair", name, revision)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	server.Metadata.Finalizers = slices.DeleteFunc(server.Metadata.Finalizers, func(value string) bool { return value == hostKeyFinalizer })
	encoded, err := server.Encode()
	if err != nil {
		return err
	}
	revision, err := strconv.ParseInt(server.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, revision)
	return err
}
