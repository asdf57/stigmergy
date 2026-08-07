//go:build integration

package etcd

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/prov-controller-test/go/internal/resource"
	storage "github.com/asdf57/prov-controller-test/go/internal/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestResourceLifecycle(t *testing.T) {
	endpoints := strings.Split(envOr("ETCD_ENDPOINTS", "http://127.0.0.1:2379"), ",")
	client, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("create etcd client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	prefix := "/homelab/test/" + time.Now().UTC().Format("20060102150405.000000000")
	store := New(client, prefix)
	t.Cleanup(func() {
		_, _ = client.Delete(context.Background(), prefix, clientv3.WithPrefix())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	created, err := store.Create(ctx, resource.Resource{
		APIVersion: resource.APIVersion,
		Kind:       "Server",
		Metadata:   resource.Metadata{Name: "integration-node"},
		Spec:       map[string]any{"hostname": "integration-node.example"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Metadata.ResourceVersion == "" || created.Metadata.UID == "" {
		t.Fatalf("server metadata was not populated: %#v", created.Metadata)
	}

	revision := mustRevision(t, created.Metadata.ResourceVersion)
	first := created
	first.Spec["hostname"] = "updated.example"
	updated, err := store.Update(ctx, first, revision)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Metadata.Generation != 2 {
		t.Fatalf("generation = %d, want 2", updated.Metadata.Generation)
	}

	stale := created
	stale.Spec = map[string]any{"hostname": "stale.example"}
	if _, err := store.Update(ctx, stale, revision); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale Update() error = %v, want conflict", err)
	}

	if err := store.Delete(ctx, updated.Kind, updated.Metadata.Name, mustRevision(t, updated.Metadata.ResourceVersion)); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, updated.Kind, updated.Metadata.Name); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get() after delete error = %v, want not found", err)
	}

	firstBulk, err := store.Create(ctx, resource.Resource{
		APIVersion: resource.APIVersion,
		Kind:       "BulkServer",
		Metadata:   resource.Metadata{Name: "bulk-one"},
		Spec:       map[string]any{},
	})
	if err != nil {
		t.Fatalf("create first bulk resource: %v", err)
	}
	secondBulk, err := store.Create(ctx, resource.Resource{
		APIVersion: resource.APIVersion,
		Kind:       "BulkServer",
		Metadata:   resource.Metadata{Name: "bulk-two"},
		Spec:       map[string]any{},
	})
	if err != nil {
		t.Fatalf("create second bulk resource: %v", err)
	}
	deleted, err := store.DeleteCollection(ctx, "BulkServer")
	if err != nil {
		t.Fatalf("DeleteCollection() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteCollection() deleted = %d, want 2", deleted)
	}
	for _, deletedResource := range []resource.Resource{firstBulk, secondBulk} {
		if _, err := store.Get(ctx, deletedResource.Kind, deletedResource.Metadata.Name); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Get() after collection delete error = %v, want not found", err)
		}
		uidResponse, err := client.Get(ctx, store.keys.uid(deletedResource.Metadata.UID))
		if err != nil {
			t.Fatalf("get UID index: %v", err)
		}
		if len(uidResponse.Kvs) != 0 {
			t.Fatalf("UID index %q was not deleted", deletedResource.Metadata.UID)
		}
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func mustRevision(t *testing.T, value string) int64 {
	t.Helper()
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse revision %q: %v", value, err)
	}
	return revision
}
