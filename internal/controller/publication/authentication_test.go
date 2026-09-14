package publication

import (
	"context"
	"strings"
	"testing"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/sshkey"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

func TestGitAuthenticationUsesReadySSHKeyPairSecret(t *testing.T) {
	storage, repository := gitAuthenticationFixture(t)

	auth, err := NewGitPublisher(storage).authentication(context.Background(), repository)
	if err != nil {
		t.Fatalf("authentication() error = %v", err)
	}
	if _, ok := auth.(*gitssh.PublicKeys); !ok {
		t.Fatalf("authentication() type = %T, want *ssh.PublicKeys", auth)
	}
}

func TestGitAuthenticationRejectsStaleSSHKeyPairStatus(t *testing.T) {
	storage, repository := gitAuthenticationFixture(t)
	key := registry.SSHKeyPairResource.Kind + "/git-ssh-key"
	keyPair := storage.resources[key]
	keyPair.Metadata.Generation++
	storage.resources[key] = keyPair

	_, err := NewGitPublisher(storage).authentication(context.Background(), repository)
	if err == nil || !strings.Contains(err.Error(), "latest generation") {
		t.Fatalf("authentication() error = %v, want stale-generation error", err)
	}
}

func TestGitAuthenticationRejectsReplacedSecret(t *testing.T) {
	storage, repository := gitAuthenticationFixture(t)
	key := registry.SecretResource.Kind + "/git-ssh-key"
	secret := storage.resources[key]
	secret.Metadata.UID = "replacement-secret-uid"
	storage.resources[key] = secret

	_, err := NewGitPublisher(storage).authentication(context.Background(), repository)
	if err == nil || !strings.Contains(err.Error(), "does not match SSHKeyPair reference UID") {
		t.Fatalf("authentication() error = %v, want Secret UID mismatch error", err)
	}
}

func TestGitAuthenticationRejectsSecretOwnedByAnotherSSHKeyPair(t *testing.T) {
	storage, repository := gitAuthenticationFixture(t)
	key := registry.SecretResource.Kind + "/git-ssh-key"
	secret := storage.resources[key]
	secret.Metadata.Annotations[sshKeyPairOwnerUIDAnnotation] = "another-key-pair-uid"
	storage.resources[key] = secret

	_, err := NewGitPublisher(storage).authentication(context.Background(), repository)
	if err == nil || !strings.Contains(err.Error(), "is not owned by SSHKeyPair") {
		t.Fatalf("authentication() error = %v, want Secret ownership error", err)
	}
}

func TestGitAuthenticationRejectsPrivateKeyWithWrongFingerprint(t *testing.T) {
	storage, repository := gitAuthenticationFixture(t)
	key := registry.SSHKeyPairResource.Kind + "/git-ssh-key"
	keyPair := storage.resources[key]
	keyPair.Status["fingerprint"] = "SHA256:not-the-stored-key"
	storage.resources[key] = keyPair

	_, err := NewGitPublisher(storage).authentication(context.Background(), repository)
	if err == nil || !strings.Contains(err.Error(), "private key does not match") {
		t.Fatalf("authentication() error = %v, want fingerprint mismatch error", err)
	}
}

func gitAuthenticationFixture(t *testing.T) (*fakeStore, apigen.GitRepositorySpec) {
	t.Helper()

	privateKey, publicKey, fingerprint, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatalf("GenerateEd25519KeyPair() error = %v", err)
	}
	generation := int64(1)
	keyPairPhase := apigen.SSHKeyPairStatusPhaseReady
	secretPhase := apigen.SecretStatusPhaseReady

	keyPair := registry.NewSSHKeyPair(resource.Metadata{
		Name: "git-ssh-key", UID: "key-pair-uid", ResourceVersion: "1", Generation: generation,
	}, apigen.SSHKeyPairSpec{
		Algorithm:      apigen.Ed25519,
		SecretStoreRef: apigen.SSHKeyPairSecretStoreReference{Name: "openbao"},
		Path:           "automation/git-ssh-key",
	})
	keyPair.Status = &apigen.SSHKeyPairStatus{
		Phase:              &keyPairPhase,
		ObservedGeneration: &generation,
		PublicKey:          &publicKey,
		Fingerprint:        &fingerprint,
		SecretRef:          &apigen.ResourceReference{Name: "git-ssh-key", Uid: "secret-uid"},
	}
	rawKeyPair, err := keyPair.Encode()
	if err != nil {
		t.Fatalf("encode SSHKeyPair: %v", err)
	}

	secret := registry.NewSecret(resource.Metadata{
		Name: "git-ssh-key", UID: "secret-uid", ResourceVersion: "1", Generation: generation,
		Annotations: map[string]string{sshKeyPairOwnerUIDAnnotation: "key-pair-uid"},
	}, apigen.SecretSpec{
		SecretStoreRef: apigen.SecretStoreReference{Name: "openbao"},
		Path:           "automation/git-ssh-key",
		Data: map[string]string{
			"privateKey":    privateKey,
			"publicKey":     publicKey,
			"sshKeyPairUID": "key-pair-uid",
		},
	})
	secret.Status = &apigen.SecretStatus{
		Phase:              &secretPhase,
		ObservedGeneration: &generation,
	}
	rawSecret, err := secret.Encode()
	if err != nil {
		t.Fatalf("encode Secret: %v", err)
	}

	return newFakeStore(rawKeyPair, rawSecret), apigen.GitRepositorySpec{
		Authentication: &apigen.GitRepositoryAuthentication{SshKeyPairRef: "git-ssh-key"},
	}
}
