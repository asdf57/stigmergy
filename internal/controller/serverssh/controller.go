package serverssh

import (
	"context"
	"errors"
	"fmt"
	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshtrust"
	"github.com/asdf57/stigmergy/internal/store"
	"sort"
	"strconv"
)

type Reconciler struct{ store store.Store }

func NewReconciler(s store.Store) *Reconciler { return &Reconciler{store: s} }
func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.ServerResource.Kind, request.Name)
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
	return r.reconcileServerKeys(ctx, server)
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
			if user.Name == "ansible" && server.Spec.SshCertificateAuthorityRef != nil {
				continue
			}
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
	if err := r.projectTrust(ctx, server, status); err != nil {
		return err
	}
	if err := r.projectBoot(ctx, server, status); err != nil {
		return err
	}
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

func (r *Reconciler) projectTrust(ctx context.Context, server registry.Server, status *apigen.ServerStatus) error {
	if server.Spec.SshCertificateAuthorityRef == nil {
		status.SshTrust = nil
		status.DesiredSSHTrustBundleDigest = nil
		status.InstalledSSHTrustBundleDigest = nil
		if status.Conditions != nil {
			conditions := []apigen.ServerCondition{}
			for _, condition := range *status.Conditions {
				if condition.Type != "SSHTrustReady" {
					conditions = append(conditions, condition)
				}
			}
			status.Conditions = &conditions
		}
		return nil
	}
	ref := server.Spec.SshCertificateAuthorityRef
	g := server.Metadata.Generation
	condition := apigen.ServerCondition{Type: "SSHTrustReady", Status: apigen.ServerConditionStatusFalse, ObservedGeneration: &g}
	setCondition := func(reason, message string) {
		condition.Reason, condition.Message = reason, &message
		conditions := []apigen.ServerCondition{}
		if status.Conditions != nil {
			for _, old := range *status.Conditions {
				if old.Type != "SSHTrustReady" {
					conditions = append(conditions, old)
				}
			}
		}
		conditions = append(conditions, condition)
		status.Conditions = &conditions
	}
	status.DesiredSSHTrustBundleDigest = nil
	raw, err := r.store.Get(ctx, registry.SSHCertificateAuthorityResource.Kind, ref.Name)
	if errors.Is(err, store.ErrNotFound) {
		setCondition("AuthorityNotFound", "The desired SSH authority does not exist")
		return nil
	}
	if err != nil {
		return err
	}
	uid := ""
	if ref.Uid != nil {
		uid = *ref.Uid
	} else if status.SshTrust != nil && status.SshTrust.AuthorityRef.Name == ref.Name {
		uid = status.SshTrust.AuthorityRef.Uid
	}
	if uid != "" && raw.Metadata.UID != uid {
		setCondition("AuthorityIdentityChanged", "Explicitly bind the replaced authority UID")
		return nil
	}
	a, err := registry.SSHCertificateAuthorityResource.Decode(raw)
	if err != nil {
		return err
	}
	if a.Metadata.DeletionTimestamp != nil || a.Status == nil || a.Status.Phase == nil || *a.Status.Phase != "Ready" || a.Status.ObservedGeneration == nil || *a.Status.ObservedGeneration != a.Metadata.Generation || a.Status.TrustBundle == nil || a.Status.TrustBundleDigest == nil {
		setCondition("AuthorityNotReady", "The desired authority has not resolved all trusted keys")
		return nil
	}
	keys := []string{}
	for _, key := range *a.Status.TrustBundle {
		keys = append(keys, key.PublicKey)
	}
	bundle, digest, err := sshtrust.Bundle(keys)
	if err != nil {
		return err
	}
	if digest != *a.Status.TrustBundleDigest {
		setCondition("BundleDigestMismatch", "Authority output is inconsistent")
		return nil
	}
	status.SshTrust = &apigen.ServerSSHTrustStatus{AuthorityRef: apigen.ResourceReference{Name: a.Metadata.Name, Uid: a.Metadata.UID}, PublicBundle: bundle}
	status.DesiredSSHTrustBundleDigest = &digest
	condition.Status = apigen.ServerConditionStatusTrue
	setCondition("TrustResolved", "The desired public bundle is ready for authenticated provisioning")
	return nil
}

func (r *Reconciler) RequestsForAuthority(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	servers, err := r.store.List(ctx, registry.ServerResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := []controller.Request{}
	for _, raw := range servers.Items {
		server, err := registry.ServerResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if server.Spec.SshCertificateAuthorityRef != nil && server.Spec.SshCertificateAuthorityRef.Name == request.Name {
			requests = append(requests, controller.Request{Kind: server.Kind, Name: server.Metadata.Name})
		}
	}
	return requests, nil
}

func (r *Reconciler) projectBoot(ctx context.Context, server registry.Server, status *apigen.ServerStatus) error {
	if server.Spec.Boot == nil {
		status.BootISORef = nil
		return nil
	}
	ref := server.Spec.Boot.IsoRef
	raw, err := r.store.Get(ctx, registry.ISOResource.Kind, ref.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	uid := ""
	if ref.Uid != nil {
		uid = *ref.Uid
	} else if status.BootISORef != nil && status.BootISORef.Name == ref.Name {
		uid = status.BootISORef.Uid
	}
	if uid != "" && raw.Metadata.UID != uid {
		return nil
	}
	if raw.Metadata.DeletionTimestamp == nil {
		status.BootISORef = &apigen.ResourceReference{Name: raw.Metadata.Name, Uid: raw.Metadata.UID}
	}
	return nil
}

func (r *Reconciler) RequestsForISO(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	servers, err := r.store.List(ctx, registry.ServerResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := []controller.Request{}
	for _, raw := range servers.Items {
		server, err := registry.ServerResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if server.Spec.Boot != nil && server.Spec.Boot.IsoRef.Name == request.Name {
			requests = append(requests, controller.Request{Kind: server.Kind, Name: server.Metadata.Name})
		}
	}
	return requests, nil
}
