package api

import (
	"context"
	"errors"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"github.com/asdf57/stigmergy/internal/testutil"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerOperationOwnership(t *testing.T) {
	held := map[string]any{"id": "worker-a", "phase": "Held"}
	released := map[string]any{"id": "worker-a", "phase": "Released"}
	server := resource.Resource{Status: map[string]any{}}
	if err := validateServerOperation(server, map[string]any{"operation": held}); err != nil {
		t.Fatal(err)
	}
	server.Status["operation"] = held
	if !activeProvisioning(server.Status) {
		t.Fatal("operation does not block provisioning")
	}
	for _, next := range []map[string]any{nil, {"id": "worker-b", "phase": "Held"}, {"id": "worker-b", "phase": "Released"}} {
		if validateServerOperation(server, map[string]any{"operation": next}) == nil {
			t.Fatal("stale/foreign owner accepted")
		}
	}
	if err := validateServerOperation(server, map[string]any{"operation": released}); err != nil {
		t.Fatal(err)
	}
	server.Status["operation"] = released
	if activeProvisioning(server.Status) {
		t.Fatal("released operation still blocks")
	}
	server.Status["provisioning"] = map[string]any{"maintenance": true}
	if validateServerOperation(server, map[string]any{"operation": map[string]any{"id": "worker-b", "phase": "Held"}}) == nil {
		t.Fatal("claim during provisioning accepted")
	}
}

func TestServerOperationStatusCAS(t *testing.T) {
	host, machine, run := runFixture()
	host.APIVersion = resource.APIVersion
	host.Spec["operatingSystem"] = map[string]any{"distribution": "arch", "version": "rolling", "architecture": "amd64", "bootMode": "uefi"}
	host.Spec["machineSelector"] = map[string]any{"location": map[string]any{"switch_mac": "00:11:22:33:44:55", "lldp_port": "1"}}
	storage := testutil.NewStore(host, machine)
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	patch := func(version, id, phase string) int {
		r := httptest.NewRequest("PATCH", "/api/v1alpha1/servers/host/status", strings.NewReader(`{"metadata":{"uid":"server-uid"},"status":{"operation":{"id":"`+id+`","phase":"`+phase+`"}}}`))
		r.Header.Set("If-Match", `"`+version+`"`)
		r.Header.Set("Content-Type", "application/merge-patch+json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code >= 500 {
			t.Log(w.Body.String())
		}
		return w.Code
	}
	if code := patch("1", "a", "Held"); code != 200 {
		t.Fatalf("claim: %d", code)
	}
	if code := patch("1", "b", "Held"); code != 409 {
		t.Fatalf("stale claim: %d", code)
	}
	latest, _ := storage.Get(context.Background(), "Server", "host")
	if code := patch(latest.Metadata.ResourceVersion, "b", "Released"); code != 409 {
		t.Fatalf("foreign release: %d", code)
	}
	api := &Server{store: storage}
	if _, err := api.createProvisioningRun(context.Background(), run); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("held operation must block provisioning: %v", err)
	}
	if _, err := storage.Get(context.Background(), "ProvisioningRun", "run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("orphan run created")
	}
	if code := patch(latest.Metadata.ResourceVersion, "a", "Released"); code != 200 {
		t.Fatalf("release: %d", code)
	}
	if _, err := api.createProvisioningRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	latest, _ = storage.Get(context.Background(), "Server", "host")
	if code := patch(latest.Metadata.ResourceVersion, "b", "Held"); code != 409 {
		t.Fatalf("provisioning reservation must block claim: %d", code)
	}
}
