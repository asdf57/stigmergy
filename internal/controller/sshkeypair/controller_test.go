package sshkeypair

import (
	"context"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/sshkey"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct{ resources map[string]resource.Resource }

func newFakeStore(resources ...resource.Resource) *fakeStore {
	result := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		result.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return result
}

func (f *fakeStore) Create(_ context.Context, candidate resource.Resource) (resource.Resource, error) {
	key := candidate.Kind + "/" + candidate.Metadata.Name
	if _, found := f.resources[key]; found {
		return resource.Resource{}, store.ErrConflict
	}
	candidate.Metadata.UID = "secret-uid"
	candidate.Metadata.Generation = 1
	candidate.Metadata.ResourceVersion = "1"
	f.resources[key] = candidate
	return candidate, nil
}

func (f *fakeStore) Get(_ context.Context, kind, name string) (resource.Resource, error) {
	value, found := f.resources[kind+"/"+name]
	if !found {
		return resource.Resource{}, store.ErrNotFound
	}
	return value, nil
}

func (f *fakeStore) List(_ context.Context, kind string) (resource.List, error) {
	result := resource.List{}
	for _, value := range f.resources {
		if value.Kind == kind {
			result.Items = append(result.Items, value)
		}
	}
	return result, nil
}

func (f *fakeStore) Update(_ context.Context, candidate resource.Resource, revision int64) (resource.Resource, error) {
	key := candidate.Kind + "/" + candidate.Metadata.Name
	existing, found := f.resources[key]
	if !found {
		return resource.Resource{}, store.ErrNotFound
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(revision, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	candidate.Metadata.UID = existing.Metadata.UID
	candidate.Metadata.DeletionTimestamp = existing.Metadata.DeletionTimestamp
	candidate.Status = existing.Status
	if existing.Metadata.DeletionTimestamp != nil && len(candidate.Metadata.Finalizers) == 0 {
		delete(f.resources, key)
		return candidate, nil
	}
	candidate.Metadata.ResourceVersion = strconv.FormatInt(revision+1, 10)
	f.resources[key] = candidate
	return candidate, nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, kind, name string, status map[string]any, revision int64) (resource.Resource, error) {
	key := kind + "/" + name
	value, found := f.resources[key]
	if !found {
		return resource.Resource{}, store.ErrNotFound
	}
	if value.Metadata.ResourceVersion != strconv.FormatInt(revision, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	value.Status = status
	value.Metadata.ResourceVersion = strconv.FormatInt(revision+1, 10)
	f.resources[key] = value
	return value, nil
}

func (f *fakeStore) Delete(_ context.Context, kind, name string, revision int64) error {
	key := kind + "/" + name
	value, found := f.resources[key]
	if !found {
		return store.ErrNotFound
	}
	if value.Metadata.ResourceVersion != strconv.FormatInt(revision, 10) {
		return store.ErrConflict
	}
	if len(value.Metadata.Finalizers) == 0 {
		delete(f.resources, key)
		return nil
	}
	now := time.Now().UTC()
	value.Metadata.DeletionTimestamp = &now
	value.Metadata.ResourceVersion = strconv.FormatInt(revision+1, 10)
	f.resources[key] = value
	return nil
}

func (*fakeStore) DeleteCollection(context.Context, string) (int64, error) { return 0, nil }
func (*fakeStore) Ready(context.Context) error                             { return nil }

func TestReconcileCreatesSecretAndPublishesReadyPublicKey(t *testing.T) {
	storage := newFakeStore(testSSHKeyPair())
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: registry.SSHKeyPairResource.Kind, Name: "ansible-homelab"}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	secretResource := storage.resources["Secret/ansible-homelab"]
	if secretResource.Spec["path"] != "automation/ansible-homelab" {
		t.Fatalf("Secret path = %#v", secretResource.Spec["path"])
	}
	if secretResource.Metadata.Annotations[ownerUIDAnnotation] != "key-pair-uid" {
		t.Fatalf("Secret ownership annotations = %#v", secretResource.Metadata.Annotations)
	}
	data := secretResource.Spec["data"].(map[string]any)
	if data["sshKeyPairUID"] != "key-pair-uid" {
		t.Fatalf("Secret ownership data = %#v", data)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(data["publicKey"].(string))); err != nil {
		t.Fatalf("generated public key is invalid: %v", err)
	}
	if phase := storage.resources["SSHKeyPair/ansible-homelab"].Status["phase"]; phase != "Pending" {
		t.Fatalf("SSHKeyPair phase before Secret readiness = %#v", phase)
	}

	secretResource.Status = map[string]any{"phase": "Ready", "observedGeneration": int64(1), "externalVersion": int64(1)}
	storage.resources["Secret/ansible-homelab"] = secretResource
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	status := storage.resources["SSHKeyPair/ansible-homelab"].Status
	if status["phase"] != "Ready" || status["publicKey"] != data["publicKey"] || status["fingerprint"] == "" {
		t.Fatalf("ready SSHKeyPair status = %#v", status)
	}
}

func TestReconcileDoesNotAdoptUnownedSecret(t *testing.T) {
	unowned := resource.Resource{
		APIVersion: registry.SecretResource.APIVersion, Kind: registry.SecretResource.Kind,
		Metadata: resource.Metadata{Name: "ansible-homelab", UID: "other", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{
			"secretStoreRef": map[string]any{"name": "openbao"}, "path": "automation/ansible-homelab", "data": map[string]any{},
		},
	}
	storage := newFakeStore(testSSHKeyPair(), unowned)
	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: registry.SSHKeyPairResource.Kind, Name: "ansible-homelab"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if phase := storage.resources["SSHKeyPair/ansible-homelab"].Status["phase"]; phase != "Conflict" {
		t.Fatalf("SSHKeyPair phase = %#v", phase)
	}
}

func TestReconcileProjectsServerDeclaredKeyPair(t *testing.T) {
	server := resource.Resource{
		APIVersion: registry.ServerResource.APIVersion, Kind: registry.ServerResource.Kind,
		Metadata: resource.Metadata{Name: "desktop", UID: "server-uid", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{
			"machineSelector": map[string]any{"location": map[string]any{"lldp_port": "bridge/ether3", "switch_mac": "d4:01:c3:27:91:67"}},
			"users": []any{map[string]any{
				"name": "ansible", "ssh": map[string]any{"authorizedKeyRefs": []any{map[string]any{"name": "ansible-homelab"}}},
			}},
		},
		Status: map[string]any{"phase": "Bound"},
	}
	keyPair := testSSHKeyPair()
	keyPair.Status = map[string]any{
		"phase": "Ready", "observedGeneration": int64(1),
		"publicKey": "ssh-ed25519 AAAA-test", "fingerprint": "SHA256:test",
	}
	storage := newFakeStore(server, keyPair)
	reconciler := NewReconciler(storage)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: registry.ServerResource.Kind, Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["Server/desktop"].Status
	if status["phase"] != "Bound" {
		t.Fatalf("Server status fields were not preserved: %#v", status)
	}
	keys := status["ssh"].(map[string]any)["authorizedKeys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("authorized keys = %#v", keys)
	}
	resolved := keys[0].(map[string]any)
	if resolved["loginUser"] != "ansible" || resolved["publicKey"] != "ssh-ed25519 AAAA-test" {
		t.Fatalf("resolved key = %#v", resolved)
	}
	reference := resolved["keyPairRef"].(map[string]any)
	if reference["name"] != "ansible-homelab" || reference["uid"] != "key-pair-uid" {
		t.Fatalf("keyPairRef = %#v", reference)
	}
}

func TestFinalizingKeyPairDeletesSecretBeforeKeyPair(t *testing.T) {
	keyPair := testSSHKeyPair()
	now := time.Now().UTC()
	keyPair.Metadata.DeletionTimestamp = &now
	secretResource := readySecret(t)
	storage := newFakeStore(keyPair, secretResource)
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: registry.SSHKeyPairResource.Kind, Name: "ansible-homelab"}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if storage.resources["Secret/ansible-homelab"].Metadata.DeletionTimestamp == nil {
		t.Fatal("Secret was not marked for deletion")
	}
	if _, found := storage.resources["SSHKeyPair/ansible-homelab"]; !found {
		t.Fatal("SSHKeyPair was deleted before Secret cleanup completed")
	}
	delete(storage.resources, "Secret/ansible-homelab")
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if _, found := storage.resources["SSHKeyPair/ansible-homelab"]; found {
		t.Fatal("SSHKeyPair still exists after Secret cleanup")
	}
}

func testSSHKeyPair() resource.Resource {
	return resource.Resource{
		APIVersion: registry.SSHKeyPairResource.APIVersion, Kind: registry.SSHKeyPairResource.Kind,
		Metadata: resource.Metadata{Name: "ansible-homelab", UID: "key-pair-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{cleanupFinalizer}},
		Spec:     map[string]any{"algorithm": "ed25519", "secretStoreRef": map[string]any{"name": "openbao"}, "path": "automation/ansible-homelab"}, Status: map[string]any{},
	}
}

func readySecret(t *testing.T) resource.Resource {
	t.Helper()
	privateKey, publicKey, _, err := sshkey.GenerateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{
		APIVersion: registry.SecretResource.APIVersion, Kind: registry.SecretResource.Kind,
		Metadata: resource.Metadata{Name: "ansible-homelab", UID: "secret-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{secretFinalizer}, Annotations: map[string]string{ownerUIDAnnotation: "key-pair-uid"}},
		Spec: map[string]any{
			"secretStoreRef": map[string]any{"name": "openbao"}, "path": "automation/ansible-homelab",
			"data": map[string]any{"privateKey": privateKey, "publicKey": publicKey, "sshKeyPairUID": "key-pair-uid"},
		},
		Status: map[string]any{"phase": "Ready", "observedGeneration": int64(1), "externalVersion": int64(1)},
	}
}

var _ store.Store = (*fakeStore)(nil)
