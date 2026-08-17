package machine

import (
	"context"
	"log/slog"

	"github.com/asdf57/prov-controller-test/go/internal/controller"
)

type MachineReportReconciler struct{}

func NewMachineReportReconciler() *MachineReportReconciler {
	return &MachineReportReconciler{}
}

func (r *MachineReportReconciler) Reconcile(ctx context.Context, event controller.Request) error {
	slog.Info("Reconciling MachineReport", "name", event.Name, "kind", event.Kind)
	return nil
}
