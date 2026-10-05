package sshcertificate

import (
	"context"
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/controller/sshauthority"
	"github.com/asdf57/stigmergy/internal/controller/sshkeypair"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshkey"
	"github.com/asdf57/stigmergy/internal/testutil"
	"golang.org/x/crypto/ssh"
)

func metadata(name string) resource.Metadata {
	return resource.Metadata{Name: name, UID: name + "-uid", Generation: 1, ResourceVersion: "1"}
}
func fixture(t *testing.T) (*Reconciler, *testutil.Store) {
	t.Helper()
	private, public, _, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, subject, _, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHKeyPair", Metadata: metadata("subject"), Spec: map[string]any{"algorithm": "ed25519", "secretStoreRef": map[string]any{"name": "bao"}, "path": "client"}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "publicKey": subject}}
	signer := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHKeyPair", Metadata: metadata("signer"), Spec: map[string]any{"algorithm": "ed25519", "secretStoreRef": map[string]any{"name": "bao"}, "path": "ca"}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "publicKey": public, "secretRef": map[string]any{"name": "signer", "uid": "signer-secret-uid"}}}
	secret := registry.NewSecret(metadata("signer"), apigen.SecretSpec{SecretStoreRef: apigen.SecretStoreReference{Name: "bao"}, Path: "ca", Data: map[string]string{"privateKey": private, "publicKey": public}})
	secret.Metadata.UID = "signer-secret-uid"
	secret.Metadata.Annotations = map[string]string{"homelab.io/ssh-key-pair-uid": "signer-uid"}
	encodedSecret, _ := secret.Encode()
	encodedSecret.Status = map[string]any{"phase": "Ready", "observedGeneration": 1}
	ca := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHCertificateAuthority", Metadata: metadata("ca"), Spec: map[string]any{"signingKeyRef": map[string]any{"name": "signer"}, "trustedKeyRefs": []any{map[string]any{"name": "signer"}}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "signingKeyRef": map[string]any{"name": "signer", "uid": "signer-uid"}, "trustBundle": []any{map[string]any{"keyPairRef": map[string]any{"name": "signer", "uid": "signer-uid"}, "publicKey": public, "fingerprint": "test"}}}}
	cert := registry.NewSSHCertificate(metadata("runner"), apigen.SSHCertificateSpec{AuthorityRef: apigen.SSHCertificateReference{Name: "ca"}, KeyPairRef: apigen.SSHCertificateReference{Name: "subject"}, Principals: []string{"ansible"}, Ttl: "24h", RenewBefore: "8h", SecretStoreRef: apigen.SSHCertificateSecretStoreReference{Name: "bao"}, Path: "runner-certificate"})
	cert.Metadata.Finalizers = []string{finalizer}
	encoded, _ := cert.Encode()
	s := testutil.NewStore(key, signer, encodedSecret, ca, encoded)
	r := NewReconciler(s)
	r.now = func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }
	return r, s
}
func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	if err := r.Reconcile(context.Background(), controller.Request{Name: "runner"}); err != nil {
		t.Fatal(err)
	}
}
func certificate(t *testing.T, s *testutil.Store) *ssh.Certificate {
	t.Helper()
	raw := s.Resources["Secret/ssh-certificate-runner"]
	output, err := registry.SecretResource.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(output.Spec.Data["value"]))
	if err != nil {
		t.Fatal(err)
	}
	return parsed.(*ssh.Certificate)
}
func publish(s *testutil.Store) {
	out := s.Resources["Secret/ssh-certificate-runner"]
	out.Status = map[string]any{"phase": "Ready", "observedGeneration": out.Metadata.Generation}
	s.Resources["Secret/ssh-certificate-runner"] = out
}

func TestIssuancePublicationNoopRenewalAndSignature(t *testing.T) {
	r, s := fixture(t)
	reconcile(t, r)
	if s.Resources["SSHCertificate/runner"].Status["phase"] != "Pending" {
		t.Fatal("must await external publication")
	}
	first := certificate(t, s)
	checker := ssh.CertChecker{Clock: r.now}
	if err := checker.CheckCert("ansible", first); err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckCert("root", first); err == nil {
		t.Fatal("unexpected root principal")
	}
	if first.CertType != ssh.UserCert || len(first.Extensions) != 1 || first.Extensions["permit-pty"] != "" {
		t.Fatal("unexpected certificate permissions")
	}
	output, _ := registry.SecretResource.Decode(s.Resources["Secret/ssh-certificate-runner"])
	if len(output.Spec.Data) != 1 || strings.Contains(output.Spec.Data["value"], "PRIVATE KEY") {
		t.Fatal("private material in output")
	}
	publish(s)
	reconcile(t, r)
	if s.Resources["SSHCertificate/runner"].Status["phase"] != "Ready" {
		t.Fatal(s.Resources["SSHCertificate/runner"].Status)
	}
	writes := s.Writes
	reconcile(t, r)
	if s.Writes != writes {
		t.Fatal("no-op reconciliation wrote resources")
	}
	initial := r.now()
	r.now = func() time.Time { return initial.Add(16 * time.Hour) }
	reconcile(t, r)
	second := certificate(t, s)
	if second.Serial == first.Serial || second.ValidBefore <= first.ValidBefore {
		t.Fatal("renewal did not rotate certificate")
	}
	if s.Resources["SSHCertificate/runner"].Status["phase"] != "Pending" {
		t.Fatal("renewal must await external Secret update")
	}
	publish(s)
	reconcile(t, r)
	if s.Resources["SSHCertificate/runner"].Status["phase"] != "Ready" {
		t.Fatal("renewal publication not observed")
	}
}

func TestDependencyAndOutputIdentityFailuresDoNotOverwriteCertificate(t *testing.T) {
	for _, scenario := range []string{"subject", "authority", "signer-secret", "signer-material", "output-owner", "path-collision", "stale-authority", "invalid-ttl", "invalid-renewal", "invalid-principal"} {
		t.Run(scenario, func(t *testing.T) {
			r, s := fixture(t)
			reconcile(t, r)
			publish(s)
			reconcile(t, r)
			before := certificate(t, s).Serial
			switch scenario {
			case "subject":
				value := s.Resources["SSHKeyPair/subject"]
				value.Metadata.UID = "replacement"
				s.Resources["SSHKeyPair/subject"] = value
			case "authority":
				value := s.Resources["SSHCertificateAuthority/ca"]
				value.Metadata.UID = "replacement"
				s.Resources["SSHCertificateAuthority/ca"] = value
			case "signer-secret":
				value := s.Resources["Secret/signer"]
				value.Metadata.UID = "replacement"
				s.Resources["Secret/signer"] = value
			case "signer-material":
				value := s.Resources["Secret/signer"]
				value.Spec["data"] = map[string]any{"privateKey": "invalid"}
				s.Resources["Secret/signer"] = value
			case "output-owner":
				value := s.Resources["Secret/ssh-certificate-runner"]
				value.Metadata.Annotations[owner] = "other"
				s.Resources["Secret/ssh-certificate-runner"] = value
			case "path-collision":
				value := registry.NewSecret(metadata("other"), apigen.SecretSpec{SecretStoreRef: apigen.SecretStoreReference{Name: "bao"}, Path: "runner-certificate", Data: map[string]string{"value": "other"}})
				raw, _ := value.Encode()
				s.Resources["Secret/other"] = raw
			case "stale-authority":
				value := s.Resources["SSHCertificateAuthority/ca"]
				value.Metadata.Generation++
				s.Resources["SSHCertificateAuthority/ca"] = value
			case "invalid-ttl":
				value := s.Resources["SSHCertificate/runner"]
				value.Spec["ttl"] = "200h"
				s.Resources["SSHCertificate/runner"] = value
			case "invalid-renewal":
				value := s.Resources["SSHCertificate/runner"]
				value.Spec["renewBefore"] = "24h"
				s.Resources["SSHCertificate/runner"] = value
			case "invalid-principal":
				value := s.Resources["SSHCertificate/runner"]
				value.Spec["principals"] = []any{"*"}
				s.Resources["SSHCertificate/runner"] = value
			}
			reconcile(t, r)
			if certificate(t, s).Serial != before {
				t.Fatal("failed validation replaced existing certificate")
			}
			if s.Resources["SSHCertificate/runner"].Status["phase"] == "Ready" {
				t.Fatal("unsafe dependency remained Ready")
			}
		})
	}
}

func TestSignerSwitchReissuesAndDeletionCleansOnlyOwnedOutput(t *testing.T) {
	r, s := fixture(t)
	reconcile(t, r)
	first := certificate(t, s)
	private, public, _, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key := s.Resources["SSHKeyPair/signer"]
	key.Metadata = metadata("new-signer")
	key.Status = map[string]any{"phase": "Ready", "observedGeneration": 1, "publicKey": public, "secretRef": map[string]any{"name": "new-signer", "uid": "new-secret-uid"}}
	s.Resources["SSHKeyPair/new-signer"] = key
	secret := registry.NewSecret(metadata("new-signer"), apigen.SecretSpec{SecretStoreRef: apigen.SecretStoreReference{Name: "bao"}, Path: "new-ca", Data: map[string]string{"privateKey": private, "publicKey": public}})
	secret.Metadata.UID = "new-secret-uid"
	secret.Metadata.Annotations = map[string]string{"homelab.io/ssh-key-pair-uid": "new-signer-uid"}
	raw, _ := secret.Encode()
	raw.Status = map[string]any{"phase": "Ready", "observedGeneration": 1}
	s.Resources["Secret/new-signer"] = raw
	ca := s.Resources["SSHCertificateAuthority/ca"]
	ca.Spec["signingKeyRef"] = map[string]any{"name": "new-signer"}
	ca.Status["signingKeyRef"] = map[string]any{"name": "new-signer", "uid": "new-signer-uid"}
	ca.Status["trustBundle"] = []any{map[string]any{"keyPairRef": map[string]any{"name": "new-signer", "uid": "new-signer-uid"}, "publicKey": public, "fingerprint": "test"}}
	s.Resources["SSHCertificateAuthority/ca"] = ca
	reconcile(t, r)
	second := certificate(t, s)
	if second.Serial == first.Serial || ssh.FingerprintSHA256(second.SignatureKey) == ssh.FingerprintSHA256(first.SignatureKey) {
		t.Fatal("signer switch did not reissue")
	}
	if err := (&ssh.CertChecker{Clock: r.now}).CheckCert("ansible", second); err != nil {
		t.Fatal(err)
	}
	cert := s.Resources["SSHCertificate/runner"]
	now := r.now()
	cert.Metadata.DeletionTimestamp = &now
	s.Resources["SSHCertificate/runner"] = cert
	reconcile(t, r)
	if s.Resources["Secret/ssh-certificate-runner"].Metadata.DeletionTimestamp == nil {
		t.Fatal("owned output not queued for external cleanup")
	}
	delete(s.Resources, "Secret/ssh-certificate-runner")
	reconcile(t, r)
	if _, ok := s.Resources["SSHCertificate/runner"]; ok {
		t.Fatal("certificate cleanup did not complete")
	}
	if _, ok := s.Resources["Secret/signer"]; !ok {
		t.Fatal("deleted CA key")
	}
}

func TestManagedCertificateProtectsAuthorityAndSubjectFromDeletion(t *testing.T) {
	r, s := fixture(t)
	reconcile(t, r)
	now := r.now()
	ca := s.Resources["SSHCertificateAuthority/ca"]
	ca.Metadata.DeletionTimestamp = &now
	ca.Metadata.Finalizers = []string{"homelab.io/ssh-authority-protection"}
	s.Resources["SSHCertificateAuthority/ca"] = ca
	if err := sshauthority.NewReconciler(s).Reconcile(context.Background(), controller.Request{Name: "ca"}); err == nil {
		t.Fatal("authority deletion ignored managed certificate consumer")
	}
	key := s.Resources["SSHKeyPair/subject"]
	key.Metadata.DeletionTimestamp = &now
	key.Metadata.Finalizers = []string{"homelab.io/ssh-key-pair-cleanup"}
	s.Resources["SSHKeyPair/subject"] = key
	if err := sshkeypair.NewReconciler(s).Reconcile(context.Background(), controller.Request{Name: "subject"}); err == nil {
		t.Fatal("subject deletion ignored managed certificate consumer")
	}
}
