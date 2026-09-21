package router

import (
	"context"
	"errors"
	"strconv"
	"testing"

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
func (*fakeStore) Update(context.Context, resource.Resource, int64) (resource.Resource, error) {
	panic("unexpected Update")
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

type probeCall struct {
	address  string
	username string
	password string
}

type fakeProber struct {
	calls []probeCall
	err   error
}

func (p *fakeProber) Probe(_ context.Context, address, username, password string) error {
	p.calls = append(p.calls, probeCall{address: address, username: username, password: password})
	return p.err
}

func TestReconcileMarksRouterReadyAfterAuthenticatedProbe(t *testing.T) {
	routerResource := testRouter("mikrotik-1", "mikrotik-creds")
	credential := testCredential("mikrotik-creds", apigen.UsernamePasswordCredentialStatusPhaseReady)
	storage := newFakeStore(routerResource, credential)
	prober := &fakeProber{}
	reconciler := NewReconcilerWithProber(storage, prober)
	request := controller.Request{Kind: registry.RouterResource.Kind, Name: routerResource.Metadata.Name}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(prober.calls) != 1 {
		t.Fatalf("Probe() calls = %d, want 1", len(prober.calls))
	}
	if got := prober.calls[0]; got.address != "10.0.0.1:8728" || got.username != "router-admin" || got.password != "secret" {
		t.Fatalf("Probe() call = %#v", got)
	}
	assertRouterStatus(t, storage, routerResource.Metadata.Name, "Ready", "APIReachable")

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if storage.statusUpdates != 1 {
		t.Fatalf("status updates = %d, want 1", storage.statusUpdates)
	}
}

func TestReconcileWaitsForCredential(t *testing.T) {
	routerResource := testRouter("mikrotik-1", "missing-creds")
	storage := newFakeStore(routerResource)
	prober := &fakeProber{}

	if err := NewReconcilerWithProber(storage, prober).Reconcile(context.Background(), controller.Request{Kind: routerResource.Kind, Name: routerResource.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(prober.calls) != 0 {
		t.Fatalf("Probe() calls = %d, want 0", len(prober.calls))
	}
	assertRouterStatus(t, storage, routerResource.Metadata.Name, "Pending", "AuthenticationNotFound")
}

func TestReconcileRecordsProbeFailureAndReturnsErrorForRetry(t *testing.T) {
	routerResource := testRouter("mikrotik-1", "mikrotik-creds")
	credential := testCredential("mikrotik-creds", apigen.UsernamePasswordCredentialStatusPhaseReady)
	storage := newFakeStore(routerResource, credential)
	prober := &fakeProber{err: errors.New("connection refused")}

	err := NewReconcilerWithProber(storage, prober).Reconcile(context.Background(), controller.Request{Kind: routerResource.Kind, Name: routerResource.Metadata.Name})
	if err == nil {
		t.Fatal("Reconcile() error = nil, want probe failure")
	}
	assertRouterStatus(t, storage, routerResource.Metadata.Name, "Failed", "APIUnreachable")
}

func TestRequestsForCredentialReturnsReferencingRouters(t *testing.T) {
	storage := newFakeStore(
		testRouter("first", "shared-creds"),
		testRouter("second", "other-creds"),
		testRouter("third", "shared-creds"),
	)

	requests, err := NewReconcilerWithProber(storage, &fakeProber{}).RequestsForCredential(context.Background(), controller.Request{
		Kind: registry.UsernamePasswordCredentialResource.Kind, Name: "shared-creds",
	})
	if err != nil {
		t.Fatalf("RequestsForCredential() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want 2", requests)
	}
	names := map[string]bool{requests[0].Name: true, requests[1].Name: true}
	if !names["first"] || !names["third"] {
		t.Fatalf("requests = %#v, want first and third", requests)
	}
}

func testRouter(name, credentialName string) resource.Resource {
	value := registry.NewRouter(resource.Metadata{
		Name: name, UID: name + "-uid", Generation: 2, ResourceVersion: "5",
	}, apigen.RouterSpec{
		Authentication: apigen.RouterAuthenticationReference{
			Type: apigen.RouterAuthenticationTypeUsernamePasswordCredential, Name: credentialName,
		},
		MgmtAddr: "10.0.0.1",
		Type:     apigen.RouterTypeMikrotik,
	})
	encoded, err := value.Encode()
	if err != nil {
		panic(err)
	}
	return encoded
}

func testCredential(name string, phase apigen.UsernamePasswordCredentialStatusPhase) resource.Resource {
	generation := int64(3)
	value := registry.NewUsernamePasswordCredential(resource.Metadata{
		Name: name, UID: name + "-uid", Generation: generation, ResourceVersion: "6",
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

func assertRouterStatus(t *testing.T, storage *fakeStore, name, phase, reason string) {
	t.Helper()
	status := storage.resources[registry.RouterResource.Kind+"/"+name].Status
	if status["phase"] != phase {
		t.Fatalf("status phase = %#v, want %q", status["phase"], phase)
	}
	conditions, ok := status["conditions"].([]any)
	if !ok || len(conditions) != 1 {
		t.Fatalf("status conditions = %#v", status["conditions"])
	}
	condition := conditions[0].(map[string]any)
	if condition["reason"] != reason {
		t.Fatalf("condition reason = %#v, want %q", condition["reason"], reason)
	}
}
