package server

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestHostKeyOwnershipProjectionAndMissingIdentity(t *testing.T) {
	ctx := context.Background()
	server := testServer("desktop", "server-uid", "ether3")
	storage := testutil.NewStore(server, resource.Resource{Kind: "SecretStore", Metadata: resource.Metadata{Name: "openbao"}})
	reconciler := NewHostKeyReconciler(storage, "openbao")
	request := controller.Request{Kind: "Server", Name: "desktop"}
	for range 2 {
		if err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	key := storage.Resources["SSHKeyPair/server-host-server-uid"]
	if key.Metadata.Annotations[hostKeyOwner] != "server-uid" || key.Spec["path"] != "ssh/hosts/server-uid" {
		t.Fatalf("bad owned key: %#v", key)
	}
	// An external observation must survive key projection and condition updates.
	value := storage.Resources["Server/desktop"]
	value.Status["hostSSH"].(map[string]any)["bootstrapPublicKey"] = "bootstrap"
	storage.Resources["Server/desktop"] = value
	key.Status = map[string]any{"phase": "Ready", "observedGeneration": float64(1), "publicKey": "public", "fingerprint": "fingerprint"}
	storage.Resources["SSHKeyPair/server-host-server-uid"] = key
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	host := storage.Resources["Server/desktop"].Status["hostSSH"].(map[string]any)
	if host["keyReady"] != true || host["bootstrapPublicKey"] != "bootstrap" || host["installedKeyPairRef"] != nil {
		t.Fatalf("bad projection: %#v", host)
	}
	writes := storage.Writes
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if storage.Writes != writes {
		t.Fatal("unchanged reconcile wrote status")
	}
	delete(storage.Resources, "SSHKeyPair/server-host-server-uid")
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, found := storage.Resources["SSHKeyPair/server-host-server-uid"]; found {
		t.Fatal("silently replaced established identity")
	}
	if storage.Resources["Server/desktop"].Status["hostSSH"].(map[string]any)["keyReady"] != false {
		t.Fatal("missing key stayed Ready")
	}
}

func TestHostKeyRejectsOwnershipAndUIDReplacement(t *testing.T) {
	for _, owner := range []string{"other-server", "server-uid"} {
		server := testServer("desktop", "server-uid", "ether3")
		server.Metadata.Finalizers = []string{hostKeyFinalizer}
		server.Status["hostSSH"] = map[string]any{"keyPairRef": map[string]any{"name": "server-host-server-uid", "uid": "previous-key"}}
		key := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHKeyPair", Metadata: resource.Metadata{Name: "server-host-server-uid", UID: "replacement-key", ResourceVersion: "1", Generation: 1, Annotations: map[string]string{hostKeyOwner: owner}}, Spec: map[string]any{"algorithm": "ed25519", "secretStoreRef": map[string]any{"name": "openbao"}, "path": "ssh/hosts/server-uid"}}
		storage := testutil.NewStore(server, key)
		if err := NewHostKeyReconciler(storage, "openbao").Reconcile(context.Background(), controller.Request{Name: "desktop"}); err != nil {
			t.Fatal(err)
		}
		if storage.Resources["Server/desktop"].Status["hostSSH"].(map[string]any)["keyReady"] != false {
			t.Fatal("adopted replacement")
		}
	}
}

func TestHostKeyFinalizerWaitsForOwnedChild(t *testing.T) {
	ctx := context.Background()
	server := testServer("desktop", "server-uid", "ether3")
	server.Metadata.Finalizers = []string{hostKeyFinalizer}
	storage := testutil.NewStore(server, resource.Resource{Kind: "SecretStore", Metadata: resource.Metadata{Name: "openbao"}})
	reconciler := NewHostKeyReconciler(storage, "openbao")
	request := controller.Request{Name: "desktop"}
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	value := storage.Resources["Server/desktop"]
	now := time.Now()
	value.Metadata.DeletionTimestamp = &now
	storage.Resources["Server/desktop"] = value
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	key := storage.Resources["SSHKeyPair/server-host-server-uid"]
	if key.Metadata.DeletionTimestamp == nil {
		t.Fatal("owned child was not deleted")
	}
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, found := storage.Resources["Server/desktop"]; !found {
		t.Fatal("parent removed before child cleanup")
	}
	// Simulate completion of the existing SSHKeyPair/Secret finalizers.
	key.Metadata.Finalizers = nil
	version, _ := strconv.ParseInt(key.Metadata.ResourceVersion, 10, 64)
	if _, err := storage.Update(ctx, key, version); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, found := storage.Resources["Server/desktop"]; found {
		t.Fatal("parent finalizer not released")
	}
}
