package machine

import (
	"context"
	"errors"
	"strconv"
	"testing"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	resources     map[string]resource.Resource
	creates       []resource.Resource
	updates       []resource.Resource
	statusUpdates []resource.Resource
	deletes       []resource.Resource
	createErr     error
	statusErr     error
	beforeDelete  func()
}

func newFakeStore(resources ...resource.Resource) *fakeStore {
	f := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		f.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return f
}

func (f *fakeStore) Create(_ context.Context, value resource.Resource) (resource.Resource, error) {
	if f.createErr != nil {
		return resource.Resource{}, f.createErr
	}
	key := value.Kind + "/" + value.Metadata.Name
	if _, exists := f.resources[key]; exists {
		return resource.Resource{}, store.ErrConflict
	}
	value.Metadata.UID = "created-uid"
	value.Metadata.Generation = 1
	value.Metadata.ResourceVersion = "1"
	f.resources[key] = value
	f.creates = append(f.creates, value)
	return value, nil
}

func (f *fakeStore) Get(_ context.Context, kind, name string) (resource.Resource, error) {
	value, exists := f.resources[kind+"/"+name]
	if !exists {
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

func (f *fakeStore) Update(_ context.Context, value resource.Resource, expectedRevision int64) (resource.Resource, error) {
	key := value.Kind + "/" + value.Metadata.Name
	existing, exists := f.resources[key]
	if !exists {
		return resource.Resource{}, store.ErrNotFound
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(expectedRevision, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	value.Metadata.ResourceVersion = strconv.FormatInt(expectedRevision+1, 10)
	value.Metadata.Generation = existing.Metadata.Generation + 1
	f.resources[key] = value
	f.updates = append(f.updates, value)
	return value, nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, kind, name string, status map[string]any, expectedRevision int64) (resource.Resource, error) {
	if f.statusErr != nil {
		return resource.Resource{}, f.statusErr
	}
	key := kind + "/" + name
	existing, exists := f.resources[key]
	if !exists {
		return resource.Resource{}, store.ErrNotFound
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(expectedRevision, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	existing.Status = status
	existing.Metadata.ResourceVersion = strconv.FormatInt(expectedRevision+1, 10)
	f.resources[key] = existing
	f.statusUpdates = append(f.statusUpdates, existing)
	return existing, nil
}

func TestMachineReportUpdatesPredeclaredMachineByLocation(t *testing.T) {
	report := testReportResource()
	machine := resource.Resource{
		APIVersion: registry.MachineResource.APIVersion,
		Kind:       registry.MachineResource.Kind,
		Metadata: resource.Metadata{
			Name:            "server-01",
			UID:             "machine-uid",
			ResourceVersion: "4",
			Generation:      1,
			Labels:          map[string]string{"homelab.io/role": "compute"},
		},
		Spec: map[string]any{"location": map[string]any{
			"lldp_port":  "Ethernet1",
			"switch_mac": "00:11:22:33:44:55",
		}},
		Status: map[string]any{"phase": "Pending"},
	}
	storage := newFakeStore(report, machine)
	reconciler := NewReconciler(storage)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: report.Kind, Name: report.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(storage.creates) != 0 {
		t.Fatalf("Create() calls = %d, want 0", len(storage.creates))
	}
	if len(storage.updates) != 0 {
		t.Fatalf("Update() calls = %d, want 0 spec updates", len(storage.updates))
	}
	if len(storage.statusUpdates) != 1 || storage.statusUpdates[0].Metadata.Name != "server-01" {
		t.Fatalf("status updates = %#v, want server-01", storage.statusUpdates)
	}
	updated := storage.resources[registry.MachineResource.Kind+"/server-01"]
	if updated.Metadata.Generation != 1 || updated.Metadata.Labels["homelab.io/role"] != "compute" {
		t.Fatalf("status update changed declared Machine metadata: %#v", updated.Metadata)
	}
	if len(updated.Spec) != 1 {
		t.Fatalf("status update changed Machine spec: %#v", updated.Spec)
	}
	if updated.Status["phase"] != "Pending" {
		t.Fatalf("inventory update did not preserve other status fields: %#v", updated.Status)
	}
}

func (f *fakeStore) Delete(_ context.Context, kind, name string, expectedRevision int64) error {
	if f.beforeDelete != nil {
		beforeDelete := f.beforeDelete
		f.beforeDelete = nil
		beforeDelete()
	}
	key := kind + "/" + name
	value, exists := f.resources[key]
	if !exists {
		return store.ErrNotFound
	}
	if value.Metadata.ResourceVersion != strconv.FormatInt(expectedRevision, 10) {
		return store.ErrConflict
	}
	delete(f.resources, key)
	f.deletes = append(f.deletes, value)
	return nil
}

func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) { return 0, nil }

func (f *fakeStore) Ready(context.Context) error { return nil }

func TestMachineReportReconcilerCreatesTypedMachineOnce(t *testing.T) {
	report := resource.Resource{
		APIVersion: registry.MachineReportResource.APIVersion,
		Kind:       registry.MachineReportResource.Kind,
		Metadata: resource.Metadata{
			Name:            "lab-node",
			UID:             "report-uid",
			ResourceVersion: "10",
		},
		Spec: map[string]any{
			"observed_at": "2026-08-18T12:00:00Z",
			"storage":     []any{},
			"system": map[string]any{
				"product_uuid":   "product-uuid",
				"product_serial": "serial-number",
			},
			"cpu":        map[string]any{"cores": []any{}},
			"interfaces": []any{},
			"lldp_info": []any{map[string]any{
				"interface": []any{map[string]any{
					"chassis": []any{map[string]any{
						"id": []any{map[string]any{"type": "mac", "value": "00:11:22:33:44:55"}},
					}},
					"port": []any{map[string]any{
						"id": []any{map[string]any{"type": "ifname", "value": "Ethernet1"}},
					}},
				}},
			}},
		},
	}
	storage := newFakeStore(report)
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: report.Kind, Name: report.Metadata.Name}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(storage.creates) != 1 {
		t.Fatalf("Create() calls = %d, want 1", len(storage.creates))
	}
	if len(storage.deletes) != 1 {
		t.Fatalf("consumed reports = %d, want 1", len(storage.deletes))
	}

	created := storage.creates[0]
	if created.Kind != registry.MachineResource.Kind {
		t.Fatalf("created Kind = %q, want %q", created.Kind, registry.MachineResource.Kind)
	}
	if _, exists := created.Spec["source_report"]; exists {
		t.Fatal("created Machine contains dangling source_report reference")
	}
	if _, exists := created.Metadata.Annotations["homelab.io/source-report-uid"]; exists {
		t.Fatal("created Machine contains dangling source-report UID annotation")
	}
	location, ok := created.Spec["location"].(map[string]any)
	if !ok {
		t.Fatalf("created location = %#v, want object", created.Spec["location"])
	}
	if location["lldp_port"] != "Ethernet1" {
		t.Fatalf("created location.lldp_port = %#v, want Ethernet1", location["lldp_port"])
	}
	if location["switch_mac"] != "00:11:22:33:44:55" {
		t.Fatalf("created location.switch_mac = %#v, want 00:11:22:33:44:55", location["switch_mac"])
	}
	if len(created.Spec) != 1 {
		t.Fatalf("created Machine spec = %#v, want location only", created.Spec)
	}
	if created.Metadata.Name != machineNameForLocation(apigen.MachineLocation{LldpPort: "Ethernet1", SwitchMac: "00:11:22:33:44:55"}) {
		t.Fatalf("created Machine name = %q, want deterministic location name", created.Metadata.Name)
	}
	if len(storage.statusUpdates) != 1 {
		t.Fatalf("UpdateStatus() calls = %d, want 1", len(storage.statusUpdates))
	}
	inventory := storage.statusUpdates[0].Status["inventory"].(map[string]any)
	if inventory["observed_at"] != "2026-08-18T12:00:00Z" {
		t.Fatalf("created Machine status.inventory.observed_at = %#v, want report timestamp", inventory["observed_at"])
	}

	report.Spec["observed_at"] = "2026-08-19T12:00:00Z"
	report.Spec["system"].(map[string]any)["product_name"] = "updated-model"
	report.Metadata.ResourceVersion = "11"
	storage.resources[report.Kind+"/"+report.Metadata.Name] = report
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("updated Reconcile() error = %v", err)
	}
	if len(storage.updates) != 0 {
		t.Fatalf("Update() calls = %d, want 0 spec updates", len(storage.updates))
	}
	if len(storage.statusUpdates) != 2 {
		t.Fatalf("UpdateStatus() calls = %d, want 2", len(storage.statusUpdates))
	}
	if len(storage.deletes) != 2 {
		t.Fatalf("consumed reports = %d, want 2", len(storage.deletes))
	}
	updatedInventory := storage.statusUpdates[1].Status["inventory"].(map[string]any)
	updatedSystem := updatedInventory["system"].(map[string]any)
	if updatedSystem["product_name"] != "updated-model" {
		t.Fatalf("updated system.product_name = %#v, want updated-model", updatedSystem["product_name"])
	}
}

func TestMachineReportIsNotConsumedWhenMachineWriteFails(t *testing.T) {
	report := testReportResource()
	storage := newFakeStore(report)
	storage.createErr = errors.New("machine write failed")
	reconciler := NewReconciler(storage)

	err := reconciler.Reconcile(context.Background(), controller.Request{Kind: report.Kind, Name: report.Metadata.Name})
	if err == nil {
		t.Fatal("Reconcile() error = nil, want machine write failure")
	}
	if len(storage.deletes) != 0 {
		t.Fatalf("consumed reports = %d, want 0", len(storage.deletes))
	}
	if _, exists := storage.resources[report.Kind+"/"+report.Metadata.Name]; !exists {
		t.Fatal("MachineReport was removed after failed Machine write")
	}
}

func TestMachineReportIsNotConsumedWhenStatusWriteFails(t *testing.T) {
	report := testReportResource()
	storage := newFakeStore(report)
	storage.statusErr = errors.New("status write failed")
	reconciler := NewReconciler(storage)

	err := reconciler.Reconcile(context.Background(), controller.Request{Kind: report.Kind, Name: report.Metadata.Name})
	if err == nil {
		t.Fatal("Reconcile() error = nil, want status write failure")
	}
	if len(storage.deletes) != 0 {
		t.Fatalf("consumed reports = %d, want 0", len(storage.deletes))
	}
	if len(storage.creates) != 1 {
		t.Fatalf("Create() calls = %d, want Machine creation to remain retryable", len(storage.creates))
	}
}

func TestMachineReportDoesNotConsumeNewerRevision(t *testing.T) {
	report := testReportResource()
	storage := newFakeStore(report)
	key := report.Kind + "/" + report.Metadata.Name
	storage.beforeDelete = func() {
		newer := storage.resources[key]
		newer.Metadata.ResourceVersion = "11"
		storage.resources[key] = newer
	}
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: report.Kind, Name: report.Metadata.Name}

	if err := reconciler.Reconcile(context.Background(), request); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want delete conflict", err)
	}
	if _, exists := storage.resources[key]; !exists {
		t.Fatal("newer MachineReport revision was consumed")
	}
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	if _, exists := storage.resources[key]; exists {
		t.Fatal("MachineReport still exists after retry")
	}
}

func TestStaleMachineReportIsConsumedWithoutRegressingMachine(t *testing.T) {
	newer := testReportResource()
	newer.Spec["observed_at"] = "2026-08-19T12:00:00Z"
	storage := newFakeStore(newer)
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: newer.Kind, Name: newer.Metadata.Name}

	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("newer Reconcile() error = %v", err)
	}
	stale := testReportResource()
	stale.Metadata.ResourceVersion = "11"
	storage.resources[stale.Kind+"/"+stale.Metadata.Name] = stale
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("stale Reconcile() error = %v", err)
	}

	if len(storage.statusUpdates) != 1 {
		t.Fatalf("UpdateStatus() calls = %d, want 1 for only the newer report", len(storage.statusUpdates))
	}
	if len(storage.deletes) != 2 {
		t.Fatalf("consumed reports = %d, want 2", len(storage.deletes))
	}
	location := apigen.MachineLocation{LldpPort: "Ethernet1", SwitchMac: "00:11:22:33:44:55"}
	machine := storage.resources[registry.MachineResource.Kind+"/"+machineNameForLocation(location)]
	inventory := machine.Status["inventory"].(map[string]any)
	if inventory["observed_at"] != "2026-08-19T12:00:00Z" {
		t.Fatalf("Machine status.inventory.observed_at = %#v, want newer timestamp", inventory["observed_at"])
	}
}

func testReportResource() resource.Resource {
	return resource.Resource{
		APIVersion: registry.MachineReportResource.APIVersion,
		Kind:       registry.MachineReportResource.Kind,
		Metadata: resource.Metadata{
			Name:            "lab-node",
			UID:             "report-uid",
			ResourceVersion: "10",
		},
		Spec: map[string]any{
			"observed_at": "2026-08-18T12:00:00Z",
			"storage":     []any{},
			"system":      map[string]any{},
			"cpu":         map[string]any{"cores": []any{}},
			"interfaces":  []any{},
			"lldp_info": []any{map[string]any{
				"interface": []any{map[string]any{
					"chassis": []any{map[string]any{
						"id": []any{map[string]any{"type": "mac", "value": "00:11:22:33:44:55"}},
					}},
					"port": []any{map[string]any{
						"id": []any{map[string]any{"type": "ifname", "value": "Ethernet1"}},
					}},
				}},
			}},
		},
	}
}
