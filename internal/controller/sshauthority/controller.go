package sshauthority

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
	"slices"
	"sort"
	"strconv"
)

type Reconciler struct{ store store.Store }

const protectionFinalizer = "homelab.io/ssh-authority-protection"

func NewReconciler(s store.Store) *Reconciler { return &Reconciler{store: s} }

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.SSHCertificateAuthorityResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if raw.Metadata.DeletionTimestamp == nil && !slices.Contains(raw.Metadata.Finalizers, protectionFinalizer) {
		raw.Metadata.Finalizers = append(raw.Metadata.Finalizers, protectionFinalizer)
		version, err := strconv.ParseInt(raw.Metadata.ResourceVersion, 10, 64)
		if err != nil {
			return err
		}
		raw, err = r.store.Update(ctx, raw, version)
		if err != nil {
			return err
		}
	}
	authority, err := registry.SSHCertificateAuthorityResource.Decode(raw)
	if err != nil {
		return err
	}
	if authority.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, authority)
	}
	status := &apigen.SSHCertificateAuthorityStatus{}
	if authority.Status != nil {
		*status = *authority.Status
	}
	fail := func(phase, reason, message string) error {
		return r.setStatus(ctx, authority, status, phase, reason, message)
	}
	signerIncluded := false
	trusted := make([]apigen.SSHAuthorityTrustedKey, 0, len(authority.Spec.TrustedKeyRefs))
	keys := make([]string, 0, len(authority.Spec.TrustedKeyRefs))
	for _, reference := range authority.Spec.TrustedKeyRefs {
		rawKey, err := r.store.Get(ctx, registry.SSHKeyPairResource.Kind, reference.Name)
		if errors.Is(err, store.ErrNotFound) {
			return fail("Pending", "KeyNotFound", fmt.Sprintf("SSHKeyPair %q does not exist", reference.Name))
		}
		if err != nil {
			return err
		}
		key, err := registry.SSHKeyPairResource.Decode(rawKey)
		if err != nil {
			return err
		}
		expectedUID := ""
		if reference.Uid != nil {
			expectedUID = *reference.Uid
		}
		if authority.Status != nil && authority.Status.TrustBundle != nil {
			for _, prior := range *authority.Status.TrustBundle {
				if prior.KeyPairRef.Name == reference.Name && expectedUID == "" {
					expectedUID = prior.KeyPairRef.Uid
				}
			}
		}
		if expectedUID != "" && expectedUID != key.Metadata.UID {
			return fail("Conflict", "KeyIdentityChanged", fmt.Sprintf("SSHKeyPair %q was replaced; explicitly bind its new UID", reference.Name))
		}
		if key.Metadata.DeletionTimestamp != nil || key.Status == nil || key.Status.Phase == nil || *key.Status.Phase != apigen.SSHKeyPairStatusPhaseReady || key.Status.ObservedGeneration == nil || *key.Status.ObservedGeneration != key.Metadata.Generation || key.Status.PublicKey == nil {
			return fail("Pending", "KeyNotReady", fmt.Sprintf("SSHKeyPair %q is not Ready at its current generation", reference.Name))
		}
		public, fingerprint, err := sshtrust.CanonicalKey(*key.Status.PublicKey)
		if err != nil {
			return fail("Failed", "InvalidPublicKey", err.Error())
		}
		if key.Status.Fingerprint == nil || *key.Status.Fingerprint != fingerprint {
			return fail("Failed", "FingerprintMismatch", fmt.Sprintf("SSHKeyPair %q public identity is inconsistent", reference.Name))
		}
		resolved := apigen.ResourceReference{Name: key.Metadata.Name, Uid: key.Metadata.UID}
		trusted = append(trusted, apigen.SSHAuthorityTrustedKey{KeyPairRef: resolved, PublicKey: public, Fingerprint: fingerprint})
		keys = append(keys, public)
		if reference.Name == authority.Spec.SigningKeyRef.Name {
			if authority.Spec.SigningKeyRef.Uid != nil && *authority.Spec.SigningKeyRef.Uid != key.Metadata.UID {
				return fail("Conflict", "SignerIdentityChanged", "signingKeyRef UID does not match the trusted signer")
			}
			status.SigningKeyRef = &resolved
			signerIncluded = true
		}
	}
	if !signerIncluded {
		return fail("Failed", "SignerNotTrusted", "signingKeyRef must appear in trustedKeyRefs")
	}
	_, digest, err := sshtrust.Bundle(keys)
	if err != nil {
		return fail("Failed", "InvalidTrustBundle", err.Error())
	}
	sort.Slice(trusted, func(i, j int) bool { return trusted[i].KeyPairRef.Name < trusted[j].KeyPairRef.Name })
	status.TrustBundle, status.TrustBundleDigest = &trusted, &digest
	return r.setStatus(ctx, authority, status, "Ready", "TrustResolved", "All referenced keys are Ready; the public trust bundle is available")
}

func (r *Reconciler) finalize(ctx context.Context, authority registry.SSHCertificateAuthority) error {
	if !slices.Contains(authority.Metadata.Finalizers, protectionFinalizer) {
		return nil
	}
	for _, kind := range []string{registry.ServerResource.Kind, registry.ISOResource.Kind, registry.SSHCertificateResource.Kind} {
		values, err := r.store.List(ctx, kind)
		if err != nil {
			return err
		}
		for _, value := range values.Items {
			field := "sshCertificateAuthorityRef"
			if kind == registry.SSHCertificateResource.Kind {
				field = "authorityRef"
			}
			if ref, ok := value.Spec[field].(map[string]any); ok && ref["name"] == authority.Metadata.Name {
				return fmt.Errorf("authority is still referenced by %s/%s", kind, value.Metadata.Name)
			}
		}
	}
	authority.Metadata.Finalizers = slices.DeleteFunc(authority.Metadata.Finalizers, func(value string) bool { return value == protectionFinalizer })
	encoded, err := authority.Encode()
	if err != nil {
		return err
	}
	version, err := strconv.ParseInt(authority.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, version)
	return err
}

func (r *Reconciler) setStatus(ctx context.Context, a registry.SSHCertificateAuthority, status *apigen.SSHCertificateAuthorityStatus, phase, reason, message string) error {
	p, g := apigen.SSHCertificateAuthorityStatusPhase(phase), a.Metadata.Generation
	state := apigen.SSHCertificateAuthorityConditionStatusFalse
	if phase == "Ready" {
		state = apigen.SSHCertificateAuthorityConditionStatusTrue
	}
	conditions := []apigen.SSHCertificateAuthorityCondition{{Type: "Ready", Status: state, Reason: reason, Message: &message, ObservedGeneration: &g}}
	status.Phase, status.ObservedGeneration, status.Conditions = &p, &g, &conditions
	if resource.EqualJSON(a.Status, status) {
		return nil
	}
	version, err := strconv.ParseInt(a.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	encoded, err := registry.SSHCertificateAuthorityResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, a.Kind, a.Metadata.Name, encoded, version)
	return err
}

func (r *Reconciler) RequestsForKey(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	authorities, err := r.store.List(ctx, registry.SSHCertificateAuthorityResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := []controller.Request{}
	for _, raw := range authorities.Items {
		a, err := registry.SSHCertificateAuthorityResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		for _, ref := range a.Spec.TrustedKeyRefs {
			if ref.Name == request.Name {
				requests = append(requests, controller.Request{Kind: a.Kind, Name: a.Metadata.Name})
				break
			}
		}
	}
	return requests, nil
}
