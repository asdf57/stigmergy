package machine

import (
	"context"
	"testing"

	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	resources map[string]resource.Resource
	creates   []resource.Resource
}

func newFakeStore(resources ...resource.Resource) *fakeStore {
	f := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		f.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return f
}

func (f *fakeStore) Create(_ context.Context, value resource.Resource) (resource.Resource, error) {
	key := value.Kind + "/" + value.Metadata.Name
	if _, exists := f.resources[key]; exists {
		return resource.Resource{}, store.ErrConflict
	}
	value.Metadata.UID = "created-uid"
	value.Metadata.Generation = 1
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

func (f *fakeStore) List(context.Context, string) (resource.List, error) {
	return resource.List{}, nil
}

func (f *fakeStore) Update(context.Context, resource.Resource, int64) (resource.Resource, error) {
	return resource.Resource{}, nil
}

func (f *fakeStore) Delete(context.Context, string, string, int64) error { return nil }

func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) { return 0, nil }

func (f *fakeStore) Ready(context.Context) error { return nil }

func TestMachineReportReconcilerCreatesTypedMachineOnce(t *testing.T) {
	report := resource.Resource{
		APIVersion: registry.MachineReportResource.APIVersion,
		Kind:       registry.MachineReportResource.Kind,
		Metadata: resource.Metadata{
			Name: "lab-node",
			UID:  "report-uid",
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
			"lldp_info":  []any{},
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

	created := storage.creates[0]
	if created.Kind != registry.MachineResource.Kind {
		t.Fatalf("created Kind = %q, want %q", created.Kind, registry.MachineResource.Kind)
	}
	if created.Spec["source_report"] != "lab-node" {
		t.Fatalf("created source_report = %#v, want lab-node", created.Spec["source_report"])
	}
	if created.Spec["product_uuid"] != "product-uuid" {
		t.Fatalf("created product_uuid = %#v, want product-uuid", created.Spec["product_uuid"])
	}
	if created.Spec["serial_number"] != "serial-number" {
		t.Fatalf("created serial_number = %#v, want serial-number", created.Spec["serial_number"])
	}
}
