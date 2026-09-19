package usernamepassword

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
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
	candidate.Metadata.Generation = existing.Metadata.Generation
	if !resource.EqualJSON(candidate.Spec, existing.Spec) {
		candidate.Metadata.Generation++
	}
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

func TestReconcileCreatesSecretAndPublishesReadyStatus(t *testing.T) {
	storage := newFakeStore(testCredential())
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: registry.UsernamePasswordCredentialResource.Kind, Name: "ansible-login"}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	secretResource := storage.resources["Secret/ansible-login"]
	if secretResource.Spec["path"] != "automation/ansible-login" {
		t.Fatalf("Secret path = %#v", secretResource.Spec["path"])
	}
	if secretResource.Metadata.Annotations[ownerUIDAnnotation] != "credential-uid" {
		t.Fatalf("Secret ownership annotations = %#v", secretResource.Metadata.Annotations)
	}
	data := secretResource.Spec["data"].(map[string]any)
	if data["username"] != "ansible" || data["password"] != "initial" || data[ownerUIDDataKey] != "credential-uid" {
		t.Fatalf("Secret data = %#v", data)
	}
	if phase := storage.resources["UsernamePasswordCredential/ansible-login"].Status["phase"]; phase != "Pending" {
		t.Fatalf("credential phase before Secret readiness = %#v", phase)
	}

	secretResource.Status = map[string]any{"phase": "Ready", "observedGeneration": int64(1), "externalVersion": int64(1)}
	storage.resources["Secret/ansible-login"] = secretResource
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	status := storage.resources["UsernamePasswordCredential/ansible-login"].Status
	if status["phase"] != "Ready" {
		t.Fatalf("ready credential status = %#v", status)
	}
	reference := status["secretRef"].(map[string]any)
	if reference["name"] != "ansible-login" || reference["uid"] != "secret-uid" {
		t.Fatalf("secretRef = %#v", reference)
	}
}

func TestReconcileUpdatesOwnedSecretWhenCredentialsChange(t *testing.T) {
	credential := testCredential()
	secretResource := readySecret()
	credential.Spec["password"] = "rotated"
	credential.Metadata.Generation = 2
	storage := newFakeStore(credential, secretResource)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: registry.UsernamePasswordCredentialResource.Kind, Name: "ansible-login"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	updatedSecret := storage.resources["Secret/ansible-login"]
	data := updatedSecret.Spec["data"].(map[string]any)
	if data["password"] != "rotated" || updatedSecret.Metadata.Generation != 2 {
		t.Fatalf("updated Secret = %#v", updatedSecret)
	}
	if phase := storage.resources["UsernamePasswordCredential/ansible-login"].Status["phase"]; phase != "Pending" {
		t.Fatalf("credential phase after rotation = %#v", phase)
	}
}

func TestReconcileUsesResourceNameUnderDefaultSecretBasePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		specPath *string
	}{
		{name: "omitted path"},
		{name: "schema default", specPath: ptrTo(defaultSecretBasePath)},
	} {
		t.Run(test.name, func(t *testing.T) {
			credential := testCredential()
			if test.specPath == nil {
				delete(credential.Spec, "path")
			} else {
				credential.Spec["path"] = *test.specPath
			}
			storage := newFakeStore(credential)

			if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: registry.UsernamePasswordCredentialResource.Kind, Name: "ansible-login"}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			want := defaultSecretBasePath + "/ansible-login"
			if path := storage.resources["Secret/ansible-login"].Spec["path"]; path != want {
				t.Fatalf("Secret path = %#v, want %q", path, want)
			}
		})
	}
}

func ptrTo[T any](value T) *T { return &value }

func TestReconcileDoesNotAdoptUnownedSecret(t *testing.T) {
	unowned := readySecret()
	unowned.Metadata.Annotations[ownerUIDAnnotation] = "somebody-else"
	storage := newFakeStore(testCredential(), unowned)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: registry.UsernamePasswordCredentialResource.Kind, Name: "ansible-login"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if phase := storage.resources["UsernamePasswordCredential/ansible-login"].Status["phase"]; phase != "Conflict" {
		t.Fatalf("credential phase = %#v", phase)
	}
}

func TestFinalizingCredentialDeletesSecretBeforeCredential(t *testing.T) {
	credential := testCredential()
	now := time.Now().UTC()
	credential.Metadata.DeletionTimestamp = &now
	storage := newFakeStore(credential, readySecret())
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: registry.UsernamePasswordCredentialResource.Kind, Name: "ansible-login"}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if storage.resources["Secret/ansible-login"].Metadata.DeletionTimestamp == nil {
		t.Fatal("Secret was not marked for deletion")
	}
	delete(storage.resources, "Secret/ansible-login")
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if _, found := storage.resources["UsernamePasswordCredential/ansible-login"]; found {
		t.Fatal("UsernamePasswordCredential still exists after Secret cleanup")
	}
}

func testCredential() resource.Resource {
	return resource.Resource{
		APIVersion: registry.UsernamePasswordCredentialResource.APIVersion, Kind: registry.UsernamePasswordCredentialResource.Kind,
		Metadata: resource.Metadata{Name: "ansible-login", UID: "credential-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{cleanupFinalizer}},
		Spec: map[string]any{
			"username": "ansible", "password": "initial", "secretStoreRef": map[string]any{"name": "openbao"}, "path": "automation/ansible-login",
		},
	}
}

func readySecret() resource.Resource {
	return resource.Resource{
		APIVersion: registry.SecretResource.APIVersion, Kind: registry.SecretResource.Kind,
		Metadata: resource.Metadata{Name: "ansible-login", UID: "secret-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{secretFinalizer}, Annotations: map[string]string{ownerUIDAnnotation: "credential-uid"}},
		Spec: map[string]any{
			"secretStoreRef": map[string]any{"name": "openbao"}, "path": "automation/ansible-login",
			"data": map[string]any{"username": "ansible", "password": "initial", ownerUIDDataKey: "credential-uid"},
		},
		Status: map[string]any{"phase": "Ready", "observedGeneration": int64(1), "externalVersion": int64(1)},
	}
}

var _ store.Store = (*fakeStore)(nil)
