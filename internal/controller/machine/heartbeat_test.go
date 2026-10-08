package machine

import (
	"context"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
)

func TestHeartbeatReceiptIsIndependentOfStaleInventoryAndMonotonic(t *testing.T) {
	report := testReportResource()
	received := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	report.Metadata.CreationTimestamp = received
	newer := testReportResource().Spec
	newer["observed_at"] = "2026-09-18T12:00:00Z"
	host := resource.Resource{APIVersion: resource.APIVersion, Kind: "Machine", Metadata: resource.Metadata{Name: "node", UID: "machine-uid", ResourceVersion: "1", Generation: 1},
		Spec:   map[string]any{"location": map[string]any{"lldp_port": "Ethernet1", "switch_mac": "00:11:22:33:44:55"}},
		Status: map[string]any{"inventory": newer, "lastSeenTime": received.Add(-time.Hour).Format(time.RFC3339)}}
	storage := newFakeStore(report, host)
	reconciler := NewReconciler(storage)
	request := controller.Request{Kind: report.Kind, Name: report.Metadata.Name}
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := storage.resources["Machine/node"]
	if updated.Status["lastSeenTime"] != received.Format(time.RFC3339) {
		t.Fatal("stale agent clock prevented recording receipt", updated.Status)
	}
	if updated.Status["inventory"].(map[string]any)["observed_at"] != newer["observed_at"] {
		t.Fatal("stale report replaced newer inventory")
	}
	report.Metadata.CreationTimestamp = received.Add(-2 * time.Hour)
	storage.resources[report.Kind+"/"+report.Metadata.Name] = report
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(storage.statusUpdates) != 1 {
		t.Fatal("old stored report replay rewrote heartbeat", storage.statusUpdates)
	}
}
