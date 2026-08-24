package sshaccess

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	resources     map[string]resource.Resource
	statusUpdates []resource.Resource
}

func newFakeStore(resources ...resource.Resource) *fakeStore {
	result := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		result.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return result
}

func (f *fakeStore) Create(context.Context, resource.Resource) (resource.Resource, error) {
	panic("unexpected Create")
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
	candidate.Metadata.CreationTimestamp = existing.Metadata.CreationTimestamp
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
	f.statusUpdates = append(f.statusUpdates, value)
	return value, nil
}
func (f *fakeStore) Delete(context.Context, string, string, int64) error { panic("unexpected Delete") }
func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) {
	panic("unexpected DeleteCollection")
}
func (f *fakeStore) Ready(context.Context) error { return nil }

type fakeKeyStore struct {
	pair          KeyPair
	err           error
	paths         []string
	owners        []KeyOwnership
	providers     []apigen.OpenBaoSecretStoreProvider
	deleteErr     error
	deletedPaths  []string
	deletedOwners []KeyOwnership
}

func (f *fakeKeyStore) DeleteKeyPair(_ context.Context, _ apigen.OpenBaoSecretStoreProvider, path string, owner KeyOwnership) error {
	f.deletedPaths = append(f.deletedPaths, path)
	f.deletedOwners = append(f.deletedOwners, owner)
	return f.deleteErr
}

func (f *fakeKeyStore) EnsureKeyPair(_ context.Context, provider apigen.OpenBaoSecretStoreProvider, path string, owner KeyOwnership) (KeyPair, error) {
	f.providers = append(f.providers, provider)
	f.paths = append(f.paths, path)
	f.owners = append(f.owners, owner)
	return f.pair, f.err
}

func TestReconcileGeneratesPerServerKeyAndProjectsPublicKey(t *testing.T) {
	storage := newFakeStore(testServer(), testSecretStore(), testGrant())
	keys := &fakeKeyStore{pair: KeyPair{PublicKey: "ssh-ed25519 AAAAtest", Fingerprint: "SHA256:test", Version: 1}}
	reconciler := NewReconcilerWithKeyStore(storage, keys)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "SSHAccessGrant", Name: "desktop-matt"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(keys.paths) != 1 || keys.paths[0] != "secrets/desktop/ssh-keys/matt" {
		t.Fatalf("key paths = %#v", keys.paths)
	}
	if keys.owners[0] != (KeyOwnership{ServerUID: "server-uid", GrantUID: "grant-uid"}) {
		t.Fatalf("ownership = %#v", keys.owners[0])
	}
	grantStatus := storage.resources["SSHAccessGrant/desktop-matt"].Status
	if grantStatus["phase"] != "Ready" || grantStatus["publicKey"] != "ssh-ed25519 AAAAtest" {
		t.Fatalf("grant status = %#v", grantStatus)
	}
	serverStatus := storage.resources["Server/desktop"].Status
	sshStatus, ok := serverStatus["ssh"].(map[string]any)
	if !ok {
		t.Fatalf("server SSH status = %#v", serverStatus["ssh"])
	}
	authorizedKeys := sshStatus["authorizedKeys"].([]any)
	if len(authorizedKeys) != 1 || authorizedKeys[0].(map[string]any)["loginUser"] != "matt" {
		t.Fatalf("authorized keys = %#v", authorizedKeys)
	}
}

func TestReconcileReportsOwnershipConflictAndRemovesStaleProjection(t *testing.T) {
	server := testServer()
	server.Status = map[string]any{"ssh": map[string]any{"authorizedKeys": []any{map[string]any{"publicKey": "stale"}}}}
	storage := newFakeStore(server, testSecretStore(), testGrant())
	keys := &fakeKeyStore{err: errors.New("OpenBao secret ownership conflict")}

	if err := NewReconcilerWithKeyStore(storage, keys).Reconcile(context.Background(), controller.Request{Kind: "SSHAccessGrant", Name: "desktop-matt"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	grantStatus := storage.resources["SSHAccessGrant/desktop-matt"].Status
	if grantStatus["phase"] != "Conflict" {
		t.Fatalf("grant status = %#v", grantStatus)
	}
	sshStatus := storage.resources["Server/desktop"].Status["ssh"].(map[string]any)
	if keys := sshStatus["authorizedKeys"].([]any); len(keys) != 0 {
		t.Fatalf("authorized keys = %#v, want empty", keys)
	}
}

func TestDeletedGrantRemovesProjectedKey(t *testing.T) {
	server := testServer()
	server.Status = map[string]any{"ssh": map[string]any{"authorizedKeys": []any{map[string]any{"publicKey": "stale"}}}}
	storage := newFakeStore(server)

	if err := NewReconcilerWithKeyStore(storage, &fakeKeyStore{}).Reconcile(context.Background(), controller.Request{Kind: "SSHAccessGrant", Name: "deleted"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sshStatus := storage.resources["Server/desktop"].Status["ssh"].(map[string]any)
	if keys := sshStatus["authorizedKeys"].([]any); len(keys) != 0 {
		t.Fatalf("authorized keys = %#v, want empty", keys)
	}
}

func TestFinalizingGrantDeletesOwnedKeyAndResource(t *testing.T) {
	server := testServer()
	server.Status = map[string]any{"ssh": map[string]any{"authorizedKeys": []any{map[string]any{"publicKey": "stale"}}}}
	grant := testGrant()
	now := time.Now().UTC()
	grant.Metadata.DeletionTimestamp = &now
	grant.Status = map[string]any{
		"phase":     "Ready",
		"serverRef": map[string]any{"name": "desktop", "uid": "server-uid"},
		"secret":    map[string]any{"logicalPath": "secrets/desktop/ssh-keys/matt"},
	}
	storage := newFakeStore(server, testSecretStore(), grant)
	keys := &fakeKeyStore{}

	if err := NewReconcilerWithKeyStore(storage, keys).Reconcile(context.Background(), controller.Request{Kind: grant.Kind, Name: grant.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, found := storage.resources["SSHAccessGrant/desktop-matt"]; found {
		t.Fatal("finalized SSHAccessGrant still exists")
	}
	if !reflect.DeepEqual(keys.deletedPaths, []string{"secrets/desktop/ssh-keys/matt"}) {
		t.Fatalf("deleted paths = %#v", keys.deletedPaths)
	}
	if !reflect.DeepEqual(keys.deletedOwners, []KeyOwnership{{ServerUID: "server-uid", GrantUID: "grant-uid"}}) {
		t.Fatalf("deleted ownership = %#v", keys.deletedOwners)
	}
	authorized := storage.resources["Server/desktop"].Status["ssh"].(map[string]any)["authorizedKeys"].([]any)
	if len(authorized) != 0 {
		t.Fatalf("authorized keys = %#v, want empty", authorized)
	}
}

func TestFinalizingGrantRetainsFinalizerWhenOpenBaoCleanupFails(t *testing.T) {
	grant := testGrant()
	now := time.Now().UTC()
	grant.Metadata.DeletionTimestamp = &now
	grant.Status = map[string]any{
		"phase":     "Ready",
		"serverRef": map[string]any{"name": "desktop", "uid": "server-uid"},
		"secret":    map[string]any{"logicalPath": "secrets/desktop/ssh-keys/matt"},
	}
	storage := newFakeStore(testServer(), testSecretStore(), grant)
	keys := &fakeKeyStore{deleteErr: errors.New("OpenBao unavailable")}

	err := NewReconcilerWithKeyStore(storage, keys).Reconcile(context.Background(), controller.Request{Kind: grant.Kind, Name: grant.Metadata.Name})
	if err == nil || !strings.Contains(err.Error(), "OpenBao unavailable") {
		t.Fatalf("Reconcile() error = %v", err)
	}
	remaining := storage.resources["SSHAccessGrant/desktop-matt"]
	if !hasFinalizer(remaining.Metadata.Finalizers, cleanupFinalizer) || remaining.Metadata.DeletionTimestamp == nil {
		t.Fatalf("terminating grant metadata = %#v", remaining.Metadata)
	}
}

func testServer() resource.Resource {
	return resource.Resource{
		APIVersion: registry.ServerResource.APIVersion, Kind: registry.ServerResource.Kind,
		Metadata: resource.Metadata{Name: "desktop", UID: "server-uid", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{"machineSelector": map[string]any{"location": map[string]any{
			"lldp_port": "bridge/ether3", "switch_mac": "d4:01:c3:27:91:67",
		}}},
		Status: map[string]any{},
	}
}

func testSecretStore() resource.Resource {
	return resource.Resource{
		APIVersion: registry.SecretStoreResource.APIVersion, Kind: registry.SecretStoreResource.Kind,
		Metadata: resource.Metadata{Name: "openbao", UID: "store-uid", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{"provider": map[string]any{"openBao": map[string]any{
			"address": "http://openbao:8200", "kvV2Mount": "kv2", "keyPrefix": "secrets",
			"authentication": map[string]any{"tokenFile": "/run/openbao/token"},
		}}},
	}
}

func testGrant() resource.Resource {
	return resource.Resource{
		APIVersion: registry.SSHAccessGrantResource.APIVersion, Kind: registry.SSHAccessGrantResource.Kind,
		Metadata: resource.Metadata{Name: "desktop-matt", UID: "grant-uid", ResourceVersion: "1", Generation: 1, Finalizers: []string{cleanupFinalizer}},
		Spec: map[string]any{
			"serverRef": map[string]any{"name": "desktop"}, "loginUser": "matt",
			"credential": map[string]any{"generatedKeyPair": map[string]any{
				"algorithm": "ed25519", "keyName": "matt", "secretStoreRef": map[string]any{"name": "openbao"},
			}},
		},
		Status: map[string]any{},
	}
}
