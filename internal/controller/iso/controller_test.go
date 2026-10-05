package iso

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/gitpublication"
	"github.com/asdf57/stigmergy/internal/isobuild"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshkey"
	"github.com/asdf57/stigmergy/internal/sshtrust"
	"github.com/asdf57/stigmergy/internal/testutil"
)

type publisher struct {
	last              string
	commits, removals int
}

func (p *publisher) Publish(_ context.Context, r gitpublication.PublishRequest) (gitpublication.PublishResult, error) {
	if r.RemoveOwned {
		p.removals++
		return gitpublication.PublishResult{}, nil
	}
	data, _ := json.Marshal(r.Artifacts)
	changed := string(data) != p.last
	if changed {
		p.commits++
		p.last = string(data)
	}
	return gitpublication.PublishResult{Revision: "git-revision", Changed: changed}, nil
}

type manifests struct{ completed *isobuild.Manifest }

func (m *manifests) Latest(_ context.Context, uid, input, digest string) (*isobuild.Manifest, error) {
	if m.completed != nil && m.completed.ISOUID == uid && m.completed.InputContent == input && m.completed.TrustBundleDigest == digest {
		return m.completed, nil
	}
	return nil, nil
}
func fixture(t *testing.T) (*Reconciler, *testutil.Store, *publisher, *manifests) {
	t.Helper()
	_, public, fp, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, digest, _ := sshtrust.Bundle([]string{public})
	metadata := func(name string) resource.Metadata {
		return resource.Metadata{Name: name, UID: name + "-uid", Generation: 1, ResourceVersion: "1"}
	}
	image := registry.NewISO(metadata("image"), apigen.ISOSpec{Distribution: "debian", Version: "trixie", Architecture: "amd64", BootMode: "uefi", SshCertificateAuthorityRef: apigen.ISOReference{Name: "ca"}, PipelineProviderRef: apigen.ISOReference{Name: "concourse"}, BuildInputs: apigen.ISOBuildInputs{RepositoryRef: apigen.ISOReference{Name: "inputs"}, Branch: "main", Path: "images/test"}})
	image.Metadata.Finalizers = []string{cleanupFinalizer}
	encoded, _ := image.Encode()
	authority := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHCertificateAuthority", Metadata: metadata("ca"), Spec: map[string]any{"signingKeyRef": map[string]any{"name": "key"}, "trustedKeyRefs": []any{map[string]any{"name": "key"}}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "trustBundleDigest": digest, "trustBundle": []any{map[string]any{"keyPairRef": map[string]any{"name": "key", "uid": "key-uid"}, "publicKey": public, "fingerprint": fp}}}}
	repo := resource.Resource{APIVersion: resource.APIVersion, Kind: "GitRepository", Metadata: metadata("inputs"), Spec: map[string]any{"url": "https://example.test/inputs.git"}}
	provider := resource.Resource{APIVersion: resource.APIVersion, Kind: "PipelineProvider", Metadata: metadata("concourse"), Spec: map[string]any{"type": "concourse", "url": "https://ci.example", "team": "main", "credentialRef": map[string]any{"name": "credentials"}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1}}
	s := testutil.NewStore(encoded, authority, repo, provider)
	p, m := &publisher{}, &manifests{}
	return NewReconciler(s, p, m, isobuild.Config{BuilderRepository: "https://example.test/builder.git", BuilderBranch: "main", DaemonRepository: "https://example.test/daemon.git", DaemonBranch: "main", PublicAPIURL: "https://api.example", ArtifactBaseURL: "https://files.example", UploadPasswordVariable: "upload"}), s, p, m
}
func TestISOPublicationCompletionRotationAndReplacement(t *testing.T) {
	r, s, p, m := fixture(t)
	ctx := context.Background()
	request := controller.Request{Name: "image"}
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	child := s.Resources["Pipeline/iso-image"]
	if child.Metadata.Annotations[ownerAnnotation] != "image-uid" || p.commits != 1 {
		t.Fatal("missing owned child or publication")
	}
	child.Status = map[string]any{"phase": "Ready", "observedGeneration": child.Metadata.Generation}
	s.Resources["Pipeline/iso-image"] = child
	status := s.Resources["ISO/image"].Status
	m.completed = &isobuild.Manifest{ISOUID: "image-uid", InputContent: status["publishedInputContent"].(string), TrustBundleDigest: status["desiredTrustBundleDigest"].(string), Artifacts: []apigen.ISOArtifact{{Type: "iso", Url: "https://files.example/image.iso"}}}
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if s.Resources["ISO/image"].Status["phase"] != "Ready" {
		t.Fatal(s.Resources["ISO/image"].Status)
	}
	writes := s.Writes
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if p.commits != 1 || s.Writes != writes {
		t.Fatal("unchanged reconciliation committed or wrote status")
	}
	// A signer-only change does not change the bundle or input snapshot.
	ca := s.Resources["SSHCertificateAuthority/ca"]
	ca.Metadata.Generation++
	ca.Status["observedGeneration"] = ca.Metadata.Generation
	ca.Spec["signingKeyRef"] = map[string]any{"name": "other"}
	s.Resources["SSHCertificateAuthority/ca"] = ca
	if err := r.Reconcile(ctx, request); err != nil || p.commits != 1 {
		t.Fatalf("signer change rebuilt: %v", err)
	}
	// Rotated trust cannot accept the prior manifest; retain the old artifacts.
	_, pub, fp, _ := sshkey.GenerateEd25519KeyPair()
	_, digest, _ := sshtrust.Bundle([]string{pub})
	ca.Metadata.Generation++
	ca.Status["observedGeneration"] = ca.Metadata.Generation
	ca.Status["trustBundleDigest"] = digest
	ca.Status["trustBundle"] = []any{map[string]any{"keyPairRef": map[string]any{"name": "other", "uid": "other-uid"}, "publicKey": pub, "fingerprint": fp}}
	s.Resources["SSHCertificateAuthority/ca"] = ca
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if p.commits != 2 || s.Resources["ISO/image"].Status["phase"] != "Pending" || s.Resources["ISO/image"].Status["artifacts"] == nil {
		t.Fatal("rotation accepted stale artifacts or hid prior successful build")
	}
	ca.Metadata.UID = "replacement"
	s.Resources["SSHCertificateAuthority/ca"] = ca
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if s.Resources["ISO/image"].Status["phase"] != "Conflict" {
		t.Fatal("replacement authority was adopted")
	}
}
func TestISODeletionAndOwnershipGuards(t *testing.T) {
	r, s, p, _ := fixture(t)
	ctx := context.Background()
	request := controller.Request{Name: "image"}
	unrelated := resource.Resource{APIVersion: resource.APIVersion, Kind: "Pipeline", Metadata: resource.Metadata{Name: "iso-image", UID: "other", ResourceVersion: "1"}, Spec: map[string]any{}}
	s.Resources["Pipeline/iso-image"] = unrelated
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if s.Resources["ISO/image"].Status["phase"] != "Conflict" {
		t.Fatal("unowned Pipeline was adopted")
	}
	image := s.Resources["ISO/image"]
	now := time.Now()
	image.Metadata.DeletionTimestamp = &now
	s.Resources["ISO/image"] = image
	if err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("unowned Pipeline was deleted")
	}
	delete(s.Resources, "Pipeline/iso-image")
	s.Resources["Server/consumer"] = resource.Resource{APIVersion: resource.APIVersion, Kind: "Server", Metadata: resource.Metadata{Name: "consumer"}, Spec: map[string]any{"boot": map[string]any{"isoRef": map[string]any{"name": "image"}}}}
	if err := r.Reconcile(ctx, request); err == nil || p.removals != 0 {
		t.Fatal("referenced ISO was cleaned")
	}
	delete(s.Resources, "Server/consumer")
	if err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Resources["ISO/image"]; ok || p.removals != 1 {
		t.Fatal("owned inputs/finalizer were not cleaned")
	}
}
