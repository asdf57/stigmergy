package server

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

func (f *fakeStore) Delete(context.Context, string, string, int64) error { panic("unexpected Delete") }
func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) {
	panic("unexpected DeleteCollection")
}
func (f *fakeStore) Ready(context.Context) error { return nil }

func TestReconcileBindsServerAndMachineByLocation(t *testing.T) {
	storage := newFakeStore(testServer("desktop", "server-uid", "ether3"), testMachine("machine-123", "machine-uid", "ether3"))
	reconciler := NewReconciler(storage)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	server := storage.resources["Server/desktop"]
	machineRef := server.Status["machineRef"].(map[string]any)
	if machineRef["name"] != "machine-123" || machineRef["uid"] != "machine-uid" {
		t.Fatalf("Server machineRef = %#v", machineRef)
	}
	if server.Status["fqdn"] != "desktop.homelab.local" || server.Status["phase"] != "Bound" {
		t.Fatalf("Server status = %#v", server.Status)
	}
	machine := storage.resources["Machine/machine-123"]
	serverRef := machine.Status["serverRef"].(map[string]any)
	if serverRef["name"] != "desktop" || serverRef["uid"] != "server-uid" || machine.Status["phase"] != "Bound" {
		t.Fatalf("Machine status = %#v", machine.Status)
	}
	if machine.Status["inventory"] != "preserved" {
		t.Fatalf("binding replaced Machine inventory: %#v", machine.Status)
	}
	updates := len(storage.statusUpdates)
	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(storage.statusUpdates) != updates {
		t.Fatalf("idempotent reconcile wrote %d additional statuses", len(storage.statusUpdates)-updates)
	}
}

func TestReconcileResolvesManagementAddressOnLocationInterface(t *testing.T) {
	server := testServer("desktop", "server-uid", "bridge/ether3")
	server.Spec["networking"] = map[string]any{
		"management": map[string]any{
			"interfaceSelector": map[string]any{"attachedAtMachineLocation": true},
			"addressSelector":   map[string]any{"family": "ipv4", "subnet": "10.1.1.0/24"},
		},
	}
	machine := testMachine("machine-123", "machine-uid", "bridge/ether3")
	machine.Status["inventory"] = map[string]any{
		"interfaces": []any{
			map[string]any{
				"name": "enp7s0", "mac": "58:11:22:19:87:78", "addresses": []any{
					map[string]any{"address": "10.1.1.251", "family": "ipv4", "prefix_length": 24},
				},
			},
			map[string]any{
				"name": "enp4s0f0np0", "mac": "98:03:9b:6a:b6:f2", "addresses": []any{
					map[string]any{"address": "10.1.1.40", "family": "ipv4", "prefix_length": 24},
				},
			},
		},
		"lldp_info": []any{map[string]any{"interface": []any{
			lldpInterface("enp7s0", "bridge/ether3"),
			lldpInterface("enp4s0f0np0", "bridge/sfp-sfpplus1"),
		}}},
	}
	storage := newFakeStore(server, machine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["Server/desktop"].Status
	management := status["networking"].(map[string]any)["management"].(map[string]any)
	if got := management["interface"].(map[string]any)["name"]; got != "enp7s0" {
		t.Fatalf("management interface = %#v, want enp7s0", got)
	}
	if got := management["address"].(map[string]any)["address"]; got != "10.1.1.251" {
		t.Fatalf("management address = %#v, want 10.1.1.251", got)
	}
}

func TestReconcileReportsAmbiguousManagementAddresses(t *testing.T) {
	server := testServer("desktop", "server-uid", "bridge/ether3")
	server.Spec["networking"] = map[string]any{
		"management": map[string]any{
			"interfaceSelector": map[string]any{"attachedAtMachineLocation": true},
			"addressSelector":   map[string]any{"family": "ipv4", "subnet": "10.1.1.0/24"},
		},
	}
	machine := testMachine("machine-123", "machine-uid", "bridge/ether3")
	machine.Status["inventory"] = map[string]any{
		"interfaces": []any{map[string]any{
			"name": "enp7s0", "mac": "58:11:22:19:87:78", "addresses": []any{
				map[string]any{"address": "10.1.1.250", "family": "ipv4", "prefix_length": 24},
				map[string]any{"address": "10.1.1.251", "family": "ipv4", "prefix_length": 24},
			},
		}},
		"lldp_info": []any{map[string]any{"interface": []any{lldpInterface("enp7s0", "bridge/ether3")}}},
	}
	storage := newFakeStore(server, machine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["Server/desktop"].Status
	if _, found := status["networking"]; found {
		t.Fatalf("ambiguous management address was published: %#v", status["networking"])
	}
	conditions := status["conditions"].([]any)
	condition := conditions[len(conditions)-1].(map[string]any)
	if condition["type"] != "ManagementAddressReady" || condition["reason"] != "ManagementAddressAmbiguous" {
		t.Fatalf("management condition = %#v", condition)
	}
}

func TestReconcileLeavesServerPendingUntilMachineExists(t *testing.T) {
	storage := newFakeStore(testServer("desktop", "server-uid", "ether3"))
	reconciler := NewReconciler(storage)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["Server/desktop"].Status
	if status["phase"] != "Pending" {
		t.Fatalf("phase = %#v, want Pending", status["phase"])
	}
	if _, found := status["machineRef"]; found {
		t.Fatalf("pending Server has machineRef: %#v", status)
	}
	condition := status["conditions"].([]any)[0].(map[string]any)
	if condition["reason"] != "NoMatchingMachine" || condition["status"] != "False" {
		t.Fatalf("MachineBound condition = %#v", condition)
	}
}

func TestReconcileDoesNotStealMachineFromAnotherServer(t *testing.T) {
	server := testServer("desktop", "desktop-uid", "ether3")
	owner := testServer("atlas", "atlas-uid", "ether3")
	machine := testMachine("machine-123", "machine-uid", "ether3")
	machine.Status["serverRef"] = map[string]any{"name": "atlas", "uid": "atlas-uid"}
	storage := newFakeStore(server, owner, machine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if got := storage.resources["Machine/machine-123"].Status["serverRef"].(map[string]any)["name"]; got != "atlas" {
		t.Fatalf("Machine was stolen by %q", got)
	}
	status := storage.resources["Server/desktop"].Status
	if status["phase"] != "Conflict" {
		t.Fatalf("phase = %#v, want Conflict", status["phase"])
	}
}

func TestReconcileReleasesOldMachineWhenSelectorChanges(t *testing.T) {
	server := testServer("desktop", "server-uid", "ether4")
	oldMachine := testMachine("machine-old", "old-uid", "ether3")
	oldMachine.Status["serverRef"] = map[string]any{"name": "desktop", "uid": "server-uid"}
	oldMachine.Status["phase"] = "Bound"
	newMachine := testMachine("machine-new", "new-uid", "ether4")
	storage := newFakeStore(server, oldMachine, newMachine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	oldStatus := storage.resources["Machine/machine-old"].Status
	if _, found := oldStatus["serverRef"]; found || oldStatus["phase"] != "Available" {
		t.Fatalf("old Machine status = %#v", oldStatus)
	}
	newStatus := storage.resources["Machine/machine-new"].Status
	if newStatus["serverRef"].(map[string]any)["name"] != "desktop" {
		t.Fatalf("new Machine status = %#v", newStatus)
	}
}

func TestDeletedServerReleasesMachine(t *testing.T) {
	machine := testMachine("machine-123", "machine-uid", "ether3")
	machine.Status["serverRef"] = map[string]any{"name": "desktop", "uid": "deleted-uid"}
	machine.Status["phase"] = "Bound"
	storage := newFakeStore(machine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources["Machine/machine-123"].Status
	if _, found := status["serverRef"]; found || status["phase"] != "Available" {
		t.Fatalf("released Machine status = %#v", status)
	}
}

func TestRecreatedServerReclaimsBindingFromItsOldUID(t *testing.T) {
	server := testServer("desktop", "new-server-uid", "ether3")
	machine := testMachine("machine-123", "machine-uid", "ether3")
	machine.Status["serverRef"] = map[string]any{"name": "desktop", "uid": "old-server-uid"}
	machine.Status["phase"] = "Bound"
	storage := newFakeStore(server, machine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	serverRef := storage.resources["Machine/machine-123"].Status["serverRef"].(map[string]any)
	if serverRef["uid"] != "new-server-uid" {
		t.Fatalf("Machine serverRef = %#v, want new Server UID", serverRef)
	}
}

func TestServerReclaimsMachineWithDanglingBinding(t *testing.T) {
	server := testServer("desktop", "desktop-uid", "ether3")
	machine := testMachine("machine-123", "machine-uid", "ether3")
	machine.Status["serverRef"] = map[string]any{"name": "deleted-server", "uid": "deleted-uid"}
	machine.Status["phase"] = "Bound"
	storage := newFakeStore(server, machine)

	if err := NewReconciler(storage).Reconcile(context.Background(), controller.Request{Kind: "Server", Name: "desktop"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	serverRef := storage.resources["Machine/machine-123"].Status["serverRef"].(map[string]any)
	if serverRef["name"] != "desktop" {
		t.Fatalf("Machine serverRef = %#v, want desktop", serverRef)
	}
}

func testServer(name, uid, port string) resource.Resource {
	return resource.Resource{
		APIVersion: registry.ServerResource.APIVersion,
		Kind:       registry.ServerResource.Kind,
		Metadata:   resource.Metadata{Name: name, UID: uid, ResourceVersion: "1", Generation: 2},
		Spec: map[string]any{
			"machineSelector": map[string]any{"location": map[string]any{"lldp_port": port, "switch_mac": "00:11:22:33:44:55"}},
			"hostName":        "desktop",
			"domainName":      "homelab.local",
		},
		Status: map[string]any{},
	}
}

func testMachine(name, uid, port string) resource.Resource {
	return resource.Resource{
		APIVersion: registry.MachineResource.APIVersion,
		Kind:       registry.MachineResource.Kind,
		Metadata:   resource.Metadata{Name: name, UID: uid, ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{"location": map[string]any{
			"lldp_port": port, "switch_mac": "00:11:22:33:44:55",
		}},
		Status: map[string]any{"inventory": "preserved", "phase": "Available"},
	}
}

func lldpInterface(name, port string) map[string]any {
	return map[string]any{
		"name": name,
		"port": []any{map[string]any{"id": []any{map[string]any{"type": "ifname", "value": port}}}},
		"chassis": []any{map[string]any{"id": []any{
			map[string]any{"type": "mac", "value": "00:11:22:33:44:55"},
		}}},
	}
}
