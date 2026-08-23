package machine

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	resources    map[string]resource.Resource
	creates      []resource.Resource
	updates      []resource.Resource
	deletes      []resource.Resource
	createErr    error
	beforeDelete func()
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
	reconciler := NewMachineReportReconciler(storage)
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
	if created.Spec["observed_at"] != "2026-08-18T12:00:00Z" {
		t.Fatalf("created observed_at = %#v, want report timestamp", created.Spec["observed_at"])
	}
	if _, ok := created.Spec["storage"]; !ok {
		t.Fatal("created Machine does not contain storage report data")
	}
	if _, ok := created.Spec["system"]; !ok {
		t.Fatal("created Machine does not contain system report data")
	}
	if _, ok := created.Spec["cpu"]; !ok {
		t.Fatal("created Machine does not contain CPU report data")
	}
	if _, ok := created.Spec["interfaces"]; !ok {
		t.Fatal("created Machine does not contain interface report data")
	}
	if _, ok := created.Spec["lldp_info"]; !ok {
		t.Fatal("created Machine does not contain LLDP report data")
	}

	report.Spec["observed_at"] = "2026-08-19T12:00:00Z"
	report.Spec["system"].(map[string]any)["product_name"] = "updated-model"
	report.Metadata.ResourceVersion = "11"
	storage.resources[report.Kind+"/"+report.Metadata.Name] = report
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("updated Reconcile() error = %v", err)
	}
	if len(storage.updates) != 1 {
		t.Fatalf("Update() calls = %d, want 1", len(storage.updates))
	}
	if len(storage.deletes) != 2 {
		t.Fatalf("consumed reports = %d, want 2", len(storage.deletes))
	}
	updatedSystem := storage.updates[0].Spec["system"].(map[string]any)
	if updatedSystem["product_name"] != "updated-model" {
		t.Fatalf("updated system.product_name = %#v, want updated-model", updatedSystem["product_name"])
	}
}

func TestMachineReportIsNotConsumedWhenMachineWriteFails(t *testing.T) {
	report := testReportResource()
	storage := newFakeStore(report)
	storage.createErr = errors.New("machine write failed")
	reconciler := NewMachineReportReconciler(storage)

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

func TestMachineReportDoesNotConsumeNewerRevision(t *testing.T) {
	report := testReportResource()
	storage := newFakeStore(report)
	key := report.Kind + "/" + report.Metadata.Name
	storage.beforeDelete = func() {
		newer := storage.resources[key]
		newer.Metadata.ResourceVersion = "11"
		storage.resources[key] = newer
	}
	reconciler := NewMachineReportReconciler(storage)
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
	reconciler := NewMachineReportReconciler(storage)
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

	if len(storage.updates) != 0 {
		t.Fatalf("Update() calls = %d, want 0 for stale report", len(storage.updates))
	}
	if len(storage.deletes) != 2 {
		t.Fatalf("consumed reports = %d, want 2", len(storage.deletes))
	}
	machine := storage.resources[registry.MachineResource.Kind+"/"+newer.Metadata.Name]
	if machine.Spec["observed_at"] != "2026-08-19T12:00:00Z" {
		t.Fatalf("Machine observed_at = %#v, want newer timestamp", machine.Spec["observed_at"])
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
			"lldp_info":   []any{},
		},
	}
}
