//go:build integration

package etcd

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/resource"
	storage "github.com/asdf57/stigmergy/internal/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestOperationCompletionIsAtomicAndPreservesTargetObservations(t *testing.T) {
	client, err := clientv3.New(clientv3.Config{Endpoints: strings.Split(envOr("ETCD_ENDPOINTS", "http://127.0.0.1:2379"), ","), DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	prefix := "/homelab/test/operations/" + time.Now().UTC().Format("20060102150405.000000000")
	t.Cleanup(func() { _, _ = client.Delete(context.Background(), prefix, clientv3.WithPrefix()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(client, prefix)
	target, err := store.Create(ctx, resource.Resource{Kind: "KubeCluster", Metadata: resource.Metadata{Name: "lab"}, Status: map[string]any{"health": "healthy"}})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := store.CreateWithStatus(ctx, resource.Resource{Kind: "Operation", Metadata: resource.Metadata{Name: "configure"}, Status: map[string]any{"phase": "Running"}}, target, func(created resource.Resource) map[string]any {
		return map[string]any{"health": "healthy", "operationRef": map[string]any{"name": created.Metadata.Name, "uid": created.Metadata.UID}}
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := store.Get(ctx, "KubeCluster", "lab")
	revision, _ := strconv.ParseInt(claimed.Metadata.ResourceVersion, 10, 64)
	observed, err := store.UpdateStatus(ctx, "KubeCluster", "lab", map[string]any{"health": "fresh"}, revision)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Status["operationRef"] == nil {
		t.Fatal("domain writer dropped coordination")
	}
	operation.Status = map[string]any{"phase": "Succeeded"}
	claimed.Status = map[string]any{"health": "healthy"}
	if _, err := store.UpdateStatusPair(ctx, operation, claimed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale pair accepted: %v", err)
	}
	stillRunning, _ := store.Get(ctx, "Operation", "configure")
	if stillRunning.Status["phase"] != "Running" {
		t.Fatal("half-committed completion")
	}
	observed.Status = map[string]any{"health": "fresh"}
	if _, err := store.UpdateStatusPair(ctx, operation, observed); err != nil {
		t.Fatal(err)
	}
	finished, _ := store.Get(ctx, "Operation", "configure")
	released, _ := store.Get(ctx, "KubeCluster", "lab")
	if finished.Metadata.ResourceVersion != released.Metadata.ResourceVersion || released.Status["operationRef"] != nil || released.Status["health"] != "fresh" || released.Metadata.Generation != target.Metadata.Generation {
		t.Fatal("completion was not atomic or lost target state")
	}
}
