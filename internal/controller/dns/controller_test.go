package dns

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

type fakeStore struct {
	resources     map[string]resource.Resource
	statusUpdates int
}

func newFakeStore(resources ...resource.Resource) *fakeStore {
	result := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		result.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return result
}

func (*fakeStore) Create(context.Context, resource.Resource) (resource.Resource, error) {
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
	f.statusUpdates++
	return value, nil
}
func (*fakeStore) Delete(context.Context, string, string, int64) error { panic("unexpected Delete") }
func (*fakeStore) DeleteCollection(context.Context, string) (int64, error) {
	panic("unexpected DeleteCollection")
}
func (*fakeStore) Ready(context.Context) error { return nil }

type ensureCall struct {
	connection Connection
	record     DesiredRecord
}

type fakeBackend struct {
	calls       []ensureCall
	deleteCalls []string
	err         error
}

func (b *fakeBackend) Ensure(_ context.Context, connection Connection, record DesiredRecord) error {
	b.calls = append(b.calls, ensureCall{connection: connection, record: record})
	return b.err
}

func (b *fakeBackend) Delete(_ context.Context, _ Connection, ownerUID string) error {
	b.deleteCalls = append(b.deleteCalls, ownerUID)
	return b.err
}

func TestReconcileProgramsRecordAndTracksBackingStore(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeA)
	routerResource := testRouter(true)
	credential := testCredential()
	storage := newFakeStore(encodeDNSRecord(record), routerResource, credential)
	backend := &fakeBackend{}
	reconciler := NewReconcilerWithBackend(storage, backend)
	request := controller.Request{Kind: registry.DNSRecordResource.Kind, Name: record.Metadata.Name}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("Ensure() calls = %d, want 1", len(backend.calls))
	}
	call := backend.calls[0]
	if call.connection.ManagementAddress != "10.0.0.1" || call.connection.Username != "router-admin" || call.connection.Password != "secret" {
		t.Fatalf("connection = %#v", call.connection)
	}
	if call.record.Name != "ansible.homelab.example.com" || call.record.Values["address"] != "10.0.2.42" || call.record.TTLSeconds != 300 {
		t.Fatalf("desired record = %#v", call.record)
	}
	status := assertDNSStatus(t, storage, record.Metadata.Name, "Ready", "RecordProgrammed")
	backingStore := status["backingStore"].(map[string]any)
	if backingStore["uid"] != routerResource.Metadata.UID || backingStore["observedGeneration"] != float64(routerResource.Metadata.Generation) {
		t.Fatalf("backingStore = %#v", backingStore)
	}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if storage.statusUpdates != 1 {
		t.Fatalf("status updates = %d, want 1", storage.statusUpdates)
	}
}

func TestReconcileDeletesOwnedRecordBeforeRemovingFinalizer(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeA)
	now := time.Now()
	record.Metadata.DeletionTimestamp = &now
	record.Metadata.Finalizers = []string{cleanupFinalizer}
	storage := newFakeStore(encodeDNSRecord(record), testRouter(true), testCredential())
	backend := &fakeBackend{}

	if err := NewReconcilerWithBackend(storage, backend).Reconcile(context.Background(), controller.Request{Kind: record.Kind, Name: record.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(backend.deleteCalls) != 1 || backend.deleteCalls[0] != record.Metadata.UID {
		t.Fatalf("Delete() calls = %#v", backend.deleteCalls)
	}
	stored := storage.resources[record.Kind+"/"+record.Metadata.Name]
	if len(stored.Metadata.Finalizers) != 0 {
		t.Fatalf("finalizers = %#v, want none", stored.Metadata.Finalizers)
	}
}

func TestReconcileReleasesFinalizerWhenRouterIsGone(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeA)
	now := time.Now()
	record.Metadata.DeletionTimestamp = &now
	record.Metadata.Finalizers = []string{cleanupFinalizer}
	storage := newFakeStore(encodeDNSRecord(record))
	backend := &fakeBackend{}

	if err := NewReconcilerWithBackend(storage, backend).Reconcile(context.Background(), controller.Request{Kind: record.Kind, Name: record.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(backend.deleteCalls) != 0 {
		t.Fatalf("Delete() calls = %#v, want none", backend.deleteCalls)
	}
	if finalizers := storage.resources[record.Kind+"/"+record.Metadata.Name].Metadata.Finalizers; len(finalizers) != 0 {
		t.Fatalf("finalizers = %#v, want none", finalizers)
	}
}

func TestReconcileWaitsForBackingStore(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeA)
	storage := newFakeStore(encodeDNSRecord(record))
	backend := &fakeBackend{}

	if err := NewReconcilerWithBackend(storage, backend).Reconcile(context.Background(), controller.Request{Kind: record.Kind, Name: record.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(backend.calls) != 0 {
		t.Fatalf("Ensure() calls = %d, want 0", len(backend.calls))
	}
	assertDNSStatus(t, storage, record.Metadata.Name, "Pending", "BackingStoreNotFound")
}

func TestReconcileWaitsForCurrentRouterGeneration(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeA)
	routerResource := testRouter(false)
	storage := newFakeStore(encodeDNSRecord(record), routerResource)
	backend := &fakeBackend{}

	if err := NewReconcilerWithBackend(storage, backend).Reconcile(context.Background(), controller.Request{Kind: record.Kind, Name: record.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertDNSStatus(t, storage, record.Metadata.Name, "Pending", "BackingStoreNotReady")
}

func TestReconcileRejectsRecordTypeUnsupportedByRouterOS(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeCAA)
	storage := newFakeStore(encodeDNSRecord(record), testRouter(true), testCredential())
	backend := &fakeBackend{}

	if err := NewReconcilerWithBackend(storage, backend).Reconcile(context.Background(), controller.Request{Kind: record.Kind, Name: record.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	assertDNSStatus(t, storage, record.Metadata.Name, "Failed", "UnsupportedRecordType")
}

func TestReconcileRecordsProgrammingFailureAndRetries(t *testing.T) {
	record := testDNSRecord(apigen.DNSRecordTypeA)
	storage := newFakeStore(encodeDNSRecord(record), testRouter(true), testCredential())
	backend := &fakeBackend{err: errors.New("permission denied")}

	err := NewReconcilerWithBackend(storage, backend).Reconcile(context.Background(), controller.Request{Kind: record.Kind, Name: record.Metadata.Name})
	if err == nil {
		t.Fatal("Reconcile() error = nil, want programming error")
	}
	assertDNSStatus(t, storage, record.Metadata.Name, "Failed", "ProgrammingFailed")
}

func TestRequestsForRouterReturnsReferencingRecords(t *testing.T) {
	first := testDNSRecord(apigen.DNSRecordTypeA)
	second := testDNSRecord(apigen.DNSRecordTypeAAAA)
	second.Metadata.Name = "ipv6"
	second.Spec.BackingStoreRef.Name = "other-router"
	encodedSecond, err := second.Encode()
	if err != nil {
		t.Fatalf("encode second DNSRecord: %v", err)
	}
	storage := newFakeStore(encodeDNSRecord(first), encodedSecond)

	requests, err := NewReconcilerWithBackend(storage, &fakeBackend{}).RequestsForRouter(context.Background(), controller.Request{
		Kind: registry.RouterResource.Kind, Name: "mikrotik-1",
	})
	if err != nil {
		t.Fatalf("RequestsForRouter() error = %v", err)
	}
	if len(requests) != 1 || requests[0].Name != first.Metadata.Name {
		t.Fatalf("requests = %#v", requests)
	}
}

func testDNSRecord(recordType apigen.DNSRecordSpecType) registry.DNSRecord {
	return registry.NewDNSRecord(resource.Metadata{
		Name: "ansible-inventory", UID: "dns-record-uid", Generation: 2, ResourceVersion: "5",
	}, apigen.DNSRecordSpec{
		BackingStoreRef: apigen.DNSRecordBackingStoreReference{Kind: registry.RouterResource.Kind, Name: "mikrotik-1"},
		Name:            "ansible", Zone: "homelab.example.com", Type: recordType, Value: "10.0.2.42", Ttl: 300,
	})
}

func encodeDNSRecord(record registry.DNSRecord) resource.Resource {
	encoded, err := record.Encode()
	if err != nil {
		panic(err)
	}
	return encoded
}

func testRouter(ready bool) resource.Resource {
	generation := int64(3)
	value := registry.NewRouter(resource.Metadata{
		Name: "mikrotik-1", UID: "router-uid", Generation: generation, ResourceVersion: "6",
	}, apigen.RouterSpec{
		Authentication: apigen.RouterAuthenticationReference{
			Type: apigen.RouterAuthenticationTypeUsernamePasswordCredential, Name: "mikrotik-creds",
		},
		MgmtAddr: "10.0.0.1", Type: apigen.RouterTypeMikrotik,
	})
	phase := apigen.RouterStatusPhasePending
	observedGeneration := generation - 1
	if ready {
		phase = apigen.RouterStatusPhaseReady
		observedGeneration = generation
	}
	value.Status = &apigen.RouterStatus{Phase: &phase, ObservedGeneration: &observedGeneration}
	encoded, err := value.Encode()
	if err != nil {
		panic(err)
	}
	return encoded
}

func testCredential() resource.Resource {
	generation := int64(4)
	phase := apigen.UsernamePasswordCredentialStatusPhaseReady
	value := registry.NewUsernamePasswordCredential(resource.Metadata{
		Name: "mikrotik-creds", UID: "credential-uid", Generation: generation, ResourceVersion: "7",
	}, apigen.UsernamePasswordCredentialSpec{
		Username: "router-admin", Password: "secret",
		SecretStoreRef: apigen.UsernamePasswordCredentialSecretStoreReference{Name: "secrets"},
	})
	value.Status = &apigen.UsernamePasswordCredentialStatus{Phase: &phase, ObservedGeneration: &generation}
	encoded, err := value.Encode()
	if err != nil {
		panic(err)
	}
	return encoded
}

func assertDNSStatus(t *testing.T, storage *fakeStore, name, phase, reason string) map[string]any {
	t.Helper()
	status := storage.resources[registry.DNSRecordResource.Kind+"/"+name].Status
	if status["phase"] != phase {
		t.Fatalf("status phase = %#v, want %q", status["phase"], phase)
	}
	conditions := status["conditions"].([]any)
	if len(conditions) != 1 || conditions[0].(map[string]any)["reason"] != reason {
		t.Fatalf("status conditions = %#v, want reason %q", conditions, reason)
	}
	return status
}
