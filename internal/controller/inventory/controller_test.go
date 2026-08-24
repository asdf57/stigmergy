package inventory

import (
	"context"
	"strconv"
	"testing"

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
	f := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		f.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return f
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
func (f *fakeStore) Update(context.Context, resource.Resource, int64) (resource.Resource, error) {
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
	f.statusUpdates = append(f.statusUpdates, value)
	return value, nil
}
func (f *fakeStore) Delete(context.Context, string, string, int64) error {
	panic("unexpected Delete")
}
func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) {
	panic("unexpected DeleteCollection")
}
func (f *fakeStore) Ready(context.Context) error { return nil }

func TestReconcileCapturesReadyServersAndReportsOmittedServers(t *testing.T) {
	group := testGroup()
	desktop := testServer("desktop", map[string]string{"homelab.io/type": "server"}, "10.1.1.251", "desktop.homelab.local")
	beelink := testServer("beelink", map[string]string{"homelab.io/type": "server"}, "", "")
	workstation := testServer("gaming", map[string]string{"homelab.io/type": "workstation"}, "10.1.1.50", "")
	storage := newFakeStore(group, desktop, beelink, workstation)
	reconciler := NewInventoryCaptureGroupReconciler(storage)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: group.Kind, Name: group.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["InventoryCaptureGroup/servers"].Status
	if status["phase"] != "Partial" || status["matchedResources"] != 2 || status["capturedResources"] != 1 {
		t.Fatalf("capture summary = %#v", status)
	}
	inventory := status["inventory"].(map[string]any)
	hosts := inventory["servers"].(map[string]any)["hosts"].(map[string]any)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %#v, want only desktop", hosts)
	}
	desktopHost := hosts["desktop"].(map[string]any)
	if desktopHost["ansible_host"] != "10.1.1.251" || desktopHost["fqdn"] != "desktop.homelab.local" {
		t.Fatalf("desktop host = %#v", desktopHost)
	}
	omitted := status["omittedResources"].([]any)
	if len(omitted) != 1 || omitted[0].(map[string]any)["name"] != "beelink" {
		t.Fatalf("omitted Servers = %#v", omitted)
	}

	updates := len(storage.statusUpdates)
	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: group.Kind, Name: group.Metadata.Name}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(storage.statusUpdates) != updates {
		t.Fatalf("idempotent reconcile wrote %d additional statuses", len(storage.statusUpdates)-updates)
	}
}

func TestRequestsForResourceReturnsEveryCaptureGroup(t *testing.T) {
	first := testGroup()
	second := testGroup()
	second.Metadata.Name = "workstations"
	storage := newFakeStore(first, second)

	requests, err := NewInventoryCaptureGroupReconciler(storage).RequestsForResource(context.Background(), controller.Request{Kind: "Server", Name: "desktop"})
	if err != nil {
		t.Fatalf("RequestsForServer() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want 2", requests)
	}
}

func TestReconcileCapturesAnyRegisteredManageableKind(t *testing.T) {
	originalDefinitions := registry.Definitions
	registry.Definitions = append(registry.Definitions, registry.Definition{
		APIVersion:   "homelab.io/v1alpha1",
		Kind:         "Router",
		StatusSchema: "RouterStatus",
	})
	defer func() { registry.Definitions = originalDefinitions }()

	group := testGroup()
	group.Spec = map[string]any{"selector": map[string]any{
		"matchKinds": []any{map[string]any{
			"apiVersion": "homelab.io/v1alpha1", "kind": "Router",
		}},
		"matchLabels": map[string]any{"homelab.io/environment": "lab"},
		"matchExpressions": []any{map[string]any{
			"key": "homelab.io/managed", "operator": "Exists",
		}},
	}}
	router := resource.Resource{
		APIVersion: "homelab.io/v1alpha1",
		Kind:       "Router",
		Metadata: resource.Metadata{
			Name: "gateway", UID: "router-uid", ResourceVersion: "1", Generation: 1,
			Labels: map[string]string{"homelab.io/environment": "lab", "homelab.io/managed": "true"},
		},
		Spec: map[string]any{},
		Status: map[string]any{"networking": map[string]any{"management": map[string]any{
			"address": map[string]any{"address": "10.1.1.1"},
		}}},
	}
	storage := newFakeStore(group, router)

	if err := NewInventoryCaptureGroupReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: group.Kind, Name: group.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["InventoryCaptureGroup/servers"].Status
	hosts := status["inventory"].(map[string]any)["servers"].(map[string]any)["hosts"].(map[string]any)
	if hosts["gateway"].(map[string]any)["ansible_host"] != "10.1.1.1" {
		t.Fatalf("Router host = %#v", hosts["gateway"])
	}
}

func testGroup() resource.Resource {
	return resource.Resource{
		APIVersion: registry.InventoryCaptureGroupResource.APIVersion,
		Kind:       registry.InventoryCaptureGroupResource.Kind,
		Metadata: resource.Metadata{
			Name: "servers", UID: "group-uid", ResourceVersion: "1", Generation: 1,
		},
		Spec: map[string]any{"selector": map[string]any{"matchLabels": map[string]any{
			"homelab.io/type": "server",
		}}},
		Status: map[string]any{},
	}
}

func testServer(name string, labels map[string]string, address, fqdn string) resource.Resource {
	status := map[string]any{
		"machineRef": map[string]any{"name": "machine-" + name, "uid": "machine-uid-" + name},
	}
	if address != "" {
		status["networking"] = map[string]any{"management": map[string]any{
			"address": map[string]any{"address": address, "family": "ipv4", "prefixLength": 24},
		}}
	}
	if fqdn != "" {
		status["fqdn"] = fqdn
	}
	return resource.Resource{
		APIVersion: registry.ServerResource.APIVersion,
		Kind:       registry.ServerResource.Kind,
		Metadata: resource.Metadata{
			Name: name, UID: "server-uid-" + name, ResourceVersion: "1", Generation: 1, Labels: labels,
		},
		Spec: map[string]any{"machineSelector": map[string]any{"location": map[string]any{
			"lldp_port": "port-" + name, "switch_mac": "00:11:22:33:44:55",
		}}},
		Status: status,
	}
}
