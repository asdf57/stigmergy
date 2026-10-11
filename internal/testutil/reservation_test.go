package testutil

import (
	"context"
	"errors"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"testing"
)

func TestReservationRejectsChangedDependencyWithoutCreatingRun(t *testing.T) {
	owner := resource.Resource{Kind: "Server", Metadata: resource.Metadata{Name: "host", UID: "host", ResourceVersion: "1"}}
	partition := resource.Resource{Kind: "DiskPartition", Metadata: resource.Metadata{Name: "root", UID: "root", ResourceVersion: "1"}}
	storage := NewStore(owner, partition)
	reviewed := partition
	partition.Metadata.ResourceVersion = "2"
	storage.Resources["DiskPartition/root"] = partition
	_, err := storage.CreateWithStatus(context.Background(), resource.Resource{Kind: "ProvisioningRun", Metadata: resource.Metadata{Name: "run"}}, owner,
		func(resource.Resource) map[string]any { return map[string]any{} }, reviewed)
	if !errors.Is(err, store.ErrConflict) || storage.Writes != 0 {
		t.Fatalf("stale dependency reservation: %v", err)
	}
}
