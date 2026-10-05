package sshauthority

import (
	"context"
	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshkey"
	"github.com/asdf57/stigmergy/internal/testutil"
	"testing"
	"time"
)

func readyKey(t *testing.T, name string) resource.Resource {
	t.Helper()
	_, public, fingerprint, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{APIVersion: "homelab.io/v1alpha1", Kind: "SSHKeyPair", Metadata: resource.Metadata{Name: name, UID: name + "-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{"algorithm": "ed25519", "path": name, "secretStoreRef": map[string]any{"name": "openbao"}}, Status: map[string]any{"phase": "Ready", "observedGeneration": int64(1), "publicKey": public, "fingerprint": fingerprint}}
}

func TestAuthorityDeletionProtectsConsumers(t *testing.T) {
	a := registry.NewSSHCertificateAuthority(resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1, ResourceVersion: "1", Finalizers: []string{protectionFinalizer}}, apigen.SSHCertificateAuthoritySpec{SigningKeyRef: apigen.SSHAuthorityKeyReference{Name: "key"}, TrustedKeyRefs: []apigen.SSHAuthorityKeyReference{{Name: "key"}}})
	now := time.Now()
	a.Metadata.DeletionTimestamp = &now
	encoded, _ := a.Encode()
	consumer := resource.Resource{Kind: "Server", Metadata: resource.Metadata{Name: "host"}, Spec: map[string]any{"sshCertificateAuthorityRef": map[string]any{"name": "ca"}}}
	s := testutil.NewStore(encoded, consumer)
	r := NewReconciler(s)
	if err := r.Reconcile(context.Background(), controller.Request{Name: "ca"}); err == nil {
		t.Fatal("referenced CA was deleted")
	}
	delete(s.Resources, "Server/host")
	if err := r.Reconcile(context.Background(), controller.Request{Name: "ca"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Resources["SSHCertificateAuthority/ca"]; ok {
		t.Fatal("unreferenced CA did not finalize")
	}
}
func TestRotationKeepsDigestForSignerOrderAndComments(t *testing.T) {
	a := registry.NewSSHCertificateAuthority(resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1, ResourceVersion: "1"}, apigen.SSHCertificateAuthoritySpec{SigningKeyRef: apigen.SSHAuthorityKeyReference{Name: "old"}, TrustedKeyRefs: []apigen.SSHAuthorityKeyReference{{Name: "old"}, {Name: "new"}}})
	encoded, _ := a.Encode()
	s := testutil.NewStore(encoded, readyKey(t, "old"), readyKey(t, "new"))
	r := NewReconciler(s)
	request := controller.Request{Kind: "SSHCertificateAuthority", Name: "ca"}
	if err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	first := s.Resources["SSHCertificateAuthority/ca"]
	digest := first.Status["trustBundleDigest"]
	if first.Status["phase"] != "Ready" {
		t.Fatal(first.Status)
	}
	writes := s.Writes
	if err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if s.Writes != writes {
		t.Fatal("unchanged reconciliation wrote status")
	}
	updated, _ := registry.SSHCertificateAuthorityResource.Decode(first)
	updated.Spec.SigningKeyRef.Name = "new"
	updated.Spec.TrustedKeyRefs = []apigen.SSHAuthorityKeyReference{{Name: "new"}, {Name: "old"}}
	updated.Metadata.Generation++
	first, _ = updated.Encode()
	s.Resources["SSHCertificateAuthority/ca"] = first
	key := s.Resources["SSHKeyPair/new"]
	key.Status["publicKey"] = key.Status["publicKey"].(string) + " changed-comment"
	s.Resources["SSHKeyPair/new"] = key
	if err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if s.Resources["SSHCertificateAuthority/ca"].Status["trustBundleDigest"] != digest {
		t.Fatal("signer, order or comment changed trust digest")
	}
	// Replacement under an unchanged name cannot silently bind another key.
	key.Metadata.UID = "replacement"
	s.Resources["SSHKeyPair/new"] = key
	if err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if s.Resources["SSHCertificateAuthority/ca"].Status["phase"] != "Conflict" {
		t.Fatal("replacement key was adopted")
	}
}
func TestMissingTrustDependencyDoesNotPublishPartialBundle(t *testing.T) {
	a := registry.NewSSHCertificateAuthority(resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1, ResourceVersion: "1"}, apigen.SSHCertificateAuthoritySpec{SigningKeyRef: apigen.SSHAuthorityKeyReference{Name: "old"}, TrustedKeyRefs: []apigen.SSHAuthorityKeyReference{{Name: "old"}, {Name: "missing"}}})
	encoded, _ := a.Encode()
	s := testutil.NewStore(encoded, readyKey(t, "old"))
	if err := NewReconciler(s).Reconcile(context.Background(), controller.Request{Name: "ca"}); err != nil {
		t.Fatal(err)
	}
	if status := s.Resources["SSHCertificateAuthority/ca"].Status; status["phase"] != "Pending" || status["trustBundle"] != nil {
		t.Fatal(status)
	}
}
