package machine

import (
	"context"

	"github.com/asdf57/prov-controller-test/go/internal/controller"
)

type MachineReportReconciler struct{}

func NewMachineReportController() *MachineReportReconciler {
	return &MachineReportReconciler{}
}

func (r *MachineReportReconciler) Reconcile(ctx context.Context, event controller.Request) error {
	return nil
}
