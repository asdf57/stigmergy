package serverssh

import (
	"context"
	"testing"

	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshkey"
	"github.com/asdf57/stigmergy/internal/sshtrust"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestTrustProjectionPinsIdentityAndClearsRemovedPolicy(t *testing.T) {
	_, pub, fp, _ := sshkey.GenerateEd25519KeyPair()
	bundle, digest, _ := sshtrust.Bundle([]string{pub})
	host := resource.Resource{APIVersion: resource.APIVersion, Kind: "Server", Metadata: resource.Metadata{Name: "host", UID: "host-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{"sshCertificateAuthorityRef": map[string]any{"name": "ca"}}}
	ca := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHCertificateAuthority", Metadata: resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "trustBundleDigest": digest, "trustBundle": []any{map[string]any{"publicKey": pub, "fingerprint": fp, "keyPairRef": map[string]any{"name": "key", "uid": "key-uid"}}}}}
	s := testutil.NewStore(host, ca)
	r := NewReconciler(s)
	ctx := context.Background()
	request := controller.Request{Name: "host"}
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	status := s.Resources["Server/host"].Status
	if status["desiredSSHTrustBundleDigest"] != digest || status["sshTrust"].(map[string]any)["publicBundle"] != bundle {
		t.Fatal(status)
	}
	ca.Metadata.UID = "replacement"
	s.Resources["SSHCertificateAuthority/ca"] = ca
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if s.Resources["Server/host"].Status["desiredSSHTrustBundleDigest"] != nil {
		t.Fatal("replacement authority adopted")
	}
	host = s.Resources["Server/host"]
	delete(host.Spec, "sshCertificateAuthorityRef")
	s.Resources["Server/host"] = host
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	status = s.Resources["Server/host"].Status
	if status["sshTrust"] != nil || status["installedSSHTrustBundleDigest"] != nil {
		t.Fatal("removed trust policy retained")
	}
	for _, value := range status["conditions"].([]any) {
		if value.(map[string]any)["type"] == "SSHTrustReady" {
			t.Fatal("stale readiness retained")
		}
	}
}
