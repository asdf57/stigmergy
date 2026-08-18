package machine

import (
	"context"
	"log/slog"

	"github.com/asdf57/prov-controller-test/go/internal/controller"
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

	// find machine reports
	reps, _ := r.store.List(ctx, "MachineReport")

	slog.Info("discovered reports", "reps", reps)
	return nil
}
