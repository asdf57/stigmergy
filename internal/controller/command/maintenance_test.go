package command

import (
	"context"
	"testing"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestMaintenanceGateIncludesEveryCaptureGroup(t *testing.T) {
	host := resource.Resource{Kind: "Server", Metadata: resource.Metadata{Name: "reserved"}, Status: map[string]any{"provisioning": map[string]any{"phase": "Installing", "maintenance": true}}}
	s := testutil.NewStore(host)
	r := &Reconciler{store: s}
	if blocked, err := r.maintenanceActive(context.Background()); err != nil || !blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	host.Status["provisioning"].(map[string]any)["maintenance"] = false
	s.Resources["Server/reserved"] = host
	if blocked, err := r.maintenanceActive(context.Background()); err != nil || blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
}
