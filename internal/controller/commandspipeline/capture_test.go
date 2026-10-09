package commandspipeline

import (
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
)

func TestUsableCapture(t *testing.T) {
	for _, test := range []struct {
		phase    string
		captured int
		observed int64
		want     bool
	}{
		{"Ready", 1, 2, true},
		{"Partial", 1, 2, true},
		{"Partial", 0, 2, false},
		{"Partial", 1, 1, false},
		{"Failed", 1, 2, false},
		{"Pending", 1, 2, false},
	} {
		inventory := map[string]apigen.InventoryCaptureAnsibleGroup{"all": {}}
		group := registry.NewInventoryCaptureGroup(resource.Metadata{Generation: 2}, apigen.InventoryCaptureGroupSpec{})
		group.Status = &apigen.InventoryCaptureGroupStatus{Phase: &test.phase, CapturedResources: &test.captured, ObservedGeneration: &test.observed, Inventory: &inventory}
		if got := usableCapture(group); got != test.want {
			t.Errorf("%+v: got %v", test, got)
		}
		group.Status.Inventory = nil
		if usableCapture(group) {
			t.Fatal("missing inventory accepted")
		}
	}
}
