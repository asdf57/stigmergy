package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type MachineReportReconciler struct {
	store store.Store
}

func NewMachineReportReconciler(store store.Store) *MachineReportReconciler {
	return &MachineReportReconciler{
		store: store,
	}
}

func (r *MachineReportReconciler) Reconcile(ctx context.Context, event controller.Request) error {
	slog.Info("Reconciling MachineReport", "name", event.Name, "kind", event.Kind)

	rsrc, err := r.store.Get(ctx, registry.MachineReportResource.Kind, event.Name)
	if errors.Is(err, store.ErrNotFound) {
		// this is an example of how you'd handle a delete case...
		// for now we do nothing
		return nil
	}
	if err != nil {
		return fmt.Errorf("get MachineReport %q: %w", event.Name, err)
	}

	report, err := registry.MachineReportResource.Decode(rsrc)
	if err != nil {
		return fmt.Errorf("decode MachineReport %q: %w", event.Name, err)
	}

	slog.Info("discovered report", "name", report.Metadata.Name, "ifaces", report.Spec.Interfaces)

	_, err = r.store.Get(ctx, registry.MachineResource.Kind, report.Metadata.Name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get Machine %q: %w", report.Metadata.Name, err)
	}

	desired := registry.Machine{
		Metadata: resource.Metadata{
			Name: report.Metadata.Name,
			Annotations: map[string]string{
				"homelab.io/source-report-uid": report.Metadata.UID,
			},
		},
		Spec: apigen.MachineSpec{
			SourceReport: report.Metadata.Name,
			ProductUuid:  report.Spec.System.ProductUuid,
			SerialNumber: report.Spec.System.ProductSerial,
		},
	}

	candidate, err := desired.Encode()
	if err != nil {
		return fmt.Errorf("encode Machine %q: %w", report.Metadata.Name, err)
	}
	if err := resource.ValidateCreate(candidate); err != nil {
		return fmt.Errorf("validate Machine %q: %w", report.Metadata.Name, err)
	}

	created, err := r.store.Create(ctx, candidate)
	if err != nil {
		return fmt.Errorf("create Machine %q: %w", report.Metadata.Name, err)
	}
	slog.Info("created Machine", "name", created.Metadata.Name, "uid", created.Metadata.UID)

	return nil
}
