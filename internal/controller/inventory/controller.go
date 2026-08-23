package inventory

import (
	"context"

	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type InventoryCaptureGroupReconciler struct {
	store store.Store
}

func NewInventoryCaptureGroupReconciler(store store.Store) *InventoryCaptureGroupReconciler {
	return &InventoryCaptureGroupReconciler{
		store: store,
	}
}

func (r *InventoryCaptureGroupReconciler) Reconcile(ctx context.Context, event controller.Request) error {

	return nil
}
