package sshaccess

import (
	"context"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
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
	candidate.Metadata.UID = "created-secret-uid"
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

func TestReconcileCreatesSecretThenProjectsReadyKey(t *testing.T) {
	storage := newFakeStore(testServer(), testSecretStore(), testGrant())
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: registry.SSHAccessGrantResource.Kind, Name: "desktop-matt"}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	secretResource := storage.resources["Secret/desktop-matt"]
	if secretResource.Spec["path"] != "desktop/ssh-keys/matt" {
		t.Fatalf("Secret path = %#v", secretResource.Spec["path"])
	}
	data := secretResource.Spec["data"].(map[string]any)
	if data["serverUID"] != "server-uid" || data["accessGrantUID"] != "grant-uid" {
		t.Fatalf("Secret ownership data = %#v", data)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(data["publicKey"].(string))); err != nil {
		t.Fatalf("generated public key is invalid: %v", err)
	}
	if phase := storage.resources["SSHAccessGrant/desktop-matt"].Status["phase"]; phase != "Pending" {
		t.Fatalf("grant phase before Secret readiness = %#v", phase)
	}

	secretResource.Status = map[string]any{"phase": "Ready", "observedGeneration": int64(1), "externalVersion": int64(3)}
	storage.resources["Secret/desktop-matt"] = secretResource
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	grantStatus := storage.resources["SSHAccessGrant/desktop-matt"].Status
	if grantStatus["phase"] != "Ready" || grantStatus["publicKey"] != data["publicKey"] {
		t.Fatalf("ready grant status = %#v", grantStatus)
	}
	keys := storage.resources["Server/desktop"].Status["ssh"].(map[string]any)["authorizedKeys"].([]any)
	if len(keys) != 1 || keys[0].(map[string]any)["loginUser"] != "matt" {
		t.Fatalf("authorized keys = %#v", keys)
	}
}

func TestReconcileDoesNotAdoptUnownedSecret(t *testing.T) {
	unowned := resource.Resource{
		APIVersion: registry.SecretResource.APIVersion, Kind: registry.SecretResource.Kind,
		Metadata: resource.Metadata{Name: "desktop-matt", UID: "other", ResourceVersion: "1", Generation: 1},
		Spec:     map[string]any{"secretStoreRef": map[string]any{"name": "openbao"}, "path": "desktop/ssh-keys/matt", "data": map[string]any{}},
	}
	storage := newFakeStore(testServer(), testSecretStore(), testGrant(), unowned)
	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Name: "desktop-matt"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if phase := storage.resources["SSHAccessGrant/desktop-matt"].Status["phase"]; phase != "Conflict" {
		t.Fatalf("grant phase = %#v", phase)
	}
}

func TestFinalizingGrantDeletesSecretBeforeGrant(t *testing.T) {
	grant := testGrant()
	now := time.Now().UTC()
	grant.Metadata.DeletionTimestamp = &now
	secretResource := readySecret(t)
	storage := newFakeStore(testServer(), testSecretStore(), grant, secretResource)
	reconciler := NewReconciler(storage)
	request := controller.Request{Name: "desktop-matt"}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if storage.resources["Secret/desktop-matt"].Metadata.DeletionTimestamp == nil {
		t.Fatal("Secret was not marked for deletion")
	}
	if _, found := storage.resources["SSHAccessGrant/desktop-matt"]; !found {
		t.Fatal("grant was deleted before Secret cleanup completed")
	}
	delete(storage.resources, "Secret/desktop-matt")
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if _, found := storage.resources["SSHAccessGrant/desktop-matt"]; found {
		t.Fatal("grant still exists after Secret cleanup")
	}
}

func readySecret(t *testing.T) resource.Resource {
	t.Helper()
	privateKey, publicKey, _, err := generateEd25519KeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return resource.Resource{
		APIVersion: registry.SecretResource.APIVersion, Kind: registry.SecretResource.Kind,
		Metadata: resource.Metadata{Name: "desktop-matt", UID: "secret-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{secretCleanupFinalizer}, Annotations: map[string]string{grantUIDAnnotation: "grant-uid", serverUIDAnnotation: "server-uid"}},
		Spec:     map[string]any{"secretStoreRef": map[string]any{"name": "openbao"}, "path": "desktop/ssh-keys/matt", "data": map[string]any{"privateKey": privateKey, "publicKey": publicKey, "serverUID": "server-uid", "accessGrantUID": "grant-uid"}},
		Status:   map[string]any{"phase": "Ready", "observedGeneration": int64(1), "externalVersion": int64(1)},
	}
}

func testServer() resource.Resource {
	return resource.Resource{
		APIVersion: registry.ServerResource.APIVersion, Kind: registry.ServerResource.Kind,
		Metadata: resource.Metadata{Name: "desktop", UID: "server-uid", ResourceVersion: "1", Generation: 1},
		Spec:     map[string]any{"machineSelector": map[string]any{"location": map[string]any{"lldp_port": "bridge/ether3", "switch_mac": "d4:01:c3:27:91:67"}}}, Status: map[string]any{},
	}
}

func testSecretStore() resource.Resource {
	return resource.Resource{
		APIVersion: registry.SecretStoreResource.APIVersion, Kind: registry.SecretStoreResource.Kind,
		Metadata: resource.Metadata{Name: "openbao", UID: "store-uid", ResourceVersion: "1", Generation: 1},
		Spec:     map[string]any{"provider": map[string]any{"openBao": map[string]any{"address": "http://openbao:8200", "kvV2Mount": "kv2", "keyPrefix": "secrets", "authentication": map[string]any{"tokenFile": "/run/openbao/token"}}}},
	}
}

func testGrant() resource.Resource {
	return resource.Resource{
		APIVersion: registry.SSHAccessGrantResource.APIVersion, Kind: registry.SSHAccessGrantResource.Kind,
		Metadata: resource.Metadata{Name: "desktop-matt", UID: "grant-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{cleanupFinalizer}},
		Spec:     map[string]any{"serverRef": map[string]any{"name": "desktop"}, "loginUser": "matt", "credential": map[string]any{"generatedKeyPair": map[string]any{"algorithm": "ed25519", "keyName": "matt", "secretStoreRef": map[string]any{"name": "openbao"}}}}, Status: map[string]any{},
	}
}

var _ store.Store = (*fakeStore)(nil)
