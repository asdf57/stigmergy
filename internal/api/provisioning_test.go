package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"github.com/asdf57/stigmergy/internal/testutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func cloneMap(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	var copy map[string]any
	_ = json.Unmarshal(data, &copy)
	return copy
}

func runFixture() (resource.Resource, resource.Resource, resource.Resource) {
	serverRef := map[string]any{"name": "host", "uid": "server-uid"}
	machineRef := map[string]any{"name": "machine", "uid": "machine-uid"}
	disk := map[string]any{"type": "disk", "serial": "serial", "wwn": "0x123", "size": float64(10000000), "model": "SSD", "tran": "sata"}
	host := resource.Resource{Kind: "Server", Metadata: resource.Metadata{Name: "host", UID: "server-uid", ResourceVersion: "1", Generation: 1}, Spec: map[string]any{"provisioning": map[string]any{"enabled": true}, "operatingSystem": map[string]any{"distribution": "arch"}}, Status: map[string]any{"machineRef": machineRef}}
	keyRef := map[string]any{"name": "host-key", "uid": "key-uid"}
	host.Status["hostSSH"] = map[string]any{"phase": "Ready", "keyReady": true, "keyPairRef": keyRef, "installedKeyPairRef": keyRef}
	machine := resource.Resource{Kind: "Machine", Metadata: resource.Metadata{Name: "machine", UID: "machine-uid", ResourceVersion: "1"}, Status: map[string]any{"serverRef": serverRef, "inventory": map[string]any{"storage": []any{disk}}}}
	run := resource.Resource{Kind: "ProvisioningRun", Metadata: resource.Metadata{Name: "run"}, Spec: map[string]any{"serverRef": serverRef, "serverGeneration": 1, "machineRef": machineRef, "storage": map[string]any{"disks": []any{map[string]any{"deviceID": "wwn:0x123", "role": "system"}}}}}
	return host, machine, run
}

func TestProvisioningRunCreationReservesAndRejectsDuplicates(t *testing.T) {
	host, machine, run := runFixture()
	storage := testutil.NewStore(host, machine)
	api := &Server{store: storage}
	created, err := api.createProvisioningRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	reserved, _ := storage.Get(context.Background(), "Server", "host")
	if object(object(reserved.Status["provisioning"])["activeRunRef"])["uid"] != created.Metadata.UID || created.Status["phase"] != "Pending" {
		t.Fatal("missing atomic reservation")
	}
	run.Metadata.Name = "second"
	if _, err := api.createProvisioningRun(context.Background(), run); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := storage.Get(context.Background(), "ProvisioningRun", "second"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("orphan duplicate created")
	}
	next := cloneMap(reserved.Spec)
	object(next["operatingSystem"])["distribution"] = "debian"
	if validateProvisioningSpec(reserved, next, true) == nil {
		t.Fatal("changed reserved OS")
	}
	next = cloneMap(reserved.Spec)
	next["reconciliation"] = map[string]any{"paused": true}
	if err := validateProvisioningSpec(reserved, next, true); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteBlockedRunReleasesOnlyItsReservation(t *testing.T) {
	for _, phase := range []string{"Pending", "PreparingBoot", "Installing", "Verifying", "Blocked"} {
		t.Run(phase, func(t *testing.T) {
			host, machine, request := runFixture()
			storage := testutil.NewStore(host, machine)
			api := &Server{store: storage}
			run, err := api.createProvisioningRun(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			run.Status["phase"] = phase
			storage.Resources["ProvisioningRun/run"] = run
			handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
			r := httptest.NewRequest("DELETE", "/api/v1alpha1/provisioning-runs/run", nil)
			r.Header.Set("If-Match", `"`+run.Metadata.ResourceVersion+`"`)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if phase != "Blocked" {
				if w.Code != http.StatusConflict {
					t.Fatalf("active delete: %d", w.Code)
				}
				return
			}
			if w.Code != http.StatusNoContent {
				t.Fatalf("blocked delete: %d %s", w.Code, w.Body.String())
			}
			owner, _ := storage.Get(context.Background(), "Server", "host")
			p := object(owner.Status["provisioning"])
			if p["maintenance"] == true || p["activeRunRef"] != nil || p["lastRunRef"] != nil {
				t.Fatal("reservation not released")
			}
			if _, err := storage.Get(context.Background(), "ProvisioningRun", "run"); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("run retained")
			}
			request.Metadata.Name = "new-run"
			if _, err := api.createProvisioningRun(context.Background(), request); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeletedHistoricalRunPreservesNewReservationAndSuccess(t *testing.T) {
	host, _, run := runFixture()
	run.Metadata.UID = "old"
	newRef := map[string]any{"name": "new", "uid": "new-uid"}
	host.Status["provisioning"] = map[string]any{"activeRunRef": newRef, "lastRunRef": newRef, "lastSuccessfulRunRef": newRef, "maintenance": true, "provisioned": true}
	if !resource.EqualJSON(host.Status, cloneStatusForRunDeletion(host.Status, run)) {
		t.Fatal("deleted another run's state")
	}
}

func TestProvisioningRunRejectsStaleBindingsAndUnsafeSelections(t *testing.T) {
	for _, mutation := range []func(*resource.Resource, *resource.Resource, *resource.Resource){
		func(h, m, r *resource.Resource) { object(r.Spec["serverRef"])["uid"] = "replacement" },
		func(h, m, r *resource.Resource) { m.Metadata.UID = "replacement" },
		func(h, m, r *resource.Resource) { m.Status["serverRef"] = nil },
		func(h, m, r *resource.Resource) { h.Metadata.Generation = 2 },
		func(h, m, r *resource.Resource) { object(h.Status["hostSSH"])["phase"] = "Pending" },
		func(h, m, r *resource.Resource) {
			object(object(m.Status["inventory"])["storage"].([]any)[0])["tran"] = "usb"
		},
		func(h, m, r *resource.Resource) {
			list := object(m.Status["inventory"])["storage"].([]any)
			object(m.Status["inventory"])["storage"] = append(list, list[0])
		},
		func(h, m, r *resource.Resource) { object(r.Spec["storage"])["disks"] = []any{} },
		func(h, m, r *resource.Resource) { h.Spec["reconciliation"] = map[string]any{"paused": true} },
	} {
		host, machine, run := runFixture()
		mutation(&host, &machine, &run)
		api := &Server{store: testutil.NewStore(host, machine)}
		if _, err := api.createProvisioningRun(context.Background(), run); err == nil {
			t.Fatal("unsafe request accepted")
		}
	}
}

func TestRunCheckpointSafetyAndRelease(t *testing.T) {
	host, machine, request := runFixture()
	storage := testutil.NewStore(host, machine)
	api := &Server{store: storage}
	run, err := api.createProvisioningRun(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	next := cloneMap(run.Status)
	next["phase"] = "PreparingBoot"
	next["attemptID"] = run.Metadata.UID
	next["snapshot"] = map[string]any{"serverUID": "server-uid", "machineRef": request.Spec["machineRef"], "diskIdentity": run.Status["selectedDisk"]}
	if err := validateProvisioningRunStatus(run, next); err != nil {
		t.Fatal(err)
	}
	run.Status = next
	bad := cloneMap(next)
	bad["phase"] = "Succeeded"
	if validateProvisioningRunStatus(run, bad) == nil {
		t.Fatal("skipped verification")
	}
	bad = cloneMap(next)
	object(bad["snapshot"])["serverUID"] = "replacement"
	if validateProvisioningRunStatus(run, bad) == nil {
		t.Fatal("changed snapshot")
	}
	for _, phase := range []string{"AwaitingLive", "Installing", "AwaitingInstalled", "Verifying", "Succeeded"} {
		next = cloneMap(run.Status)
		next["phase"] = phase
		if phase == "Succeeded" {
			next["maintenance"] = false
			next["netbootArmed"] = false
			next["completedAt"] = "2026-10-07T00:00:00Z"
		}
		if err := validateProvisioningRunStatus(run, next); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		run.Status = next
	}
	reserved, _ := storage.Get(context.Background(), "Server", "host")
	released := applyJSONMergePatch(reserved.Status, map[string]any{"provisioning": map[string]any{"activeRunRef": nil, "maintenance": false, "provisioned": true, "lastSuccessfulRunRef": object(reserved.Status["provisioning"])["activeRunRef"]}})
	if api.validateProvisioningStatus(context.Background(), reserved, released) == nil {
		t.Fatal("released unfinished run")
	}
	storage.Resources["ProvisioningRun/run"] = run
	if err := api.validateProvisioningStatus(context.Background(), reserved, released); err != nil {
		t.Fatal(err)
	}
	bad = cloneMap(run.Status)
	bad["phase"] = "PreparingBoot"
	if validateProvisioningRunStatus(run, bad) == nil {
		t.Fatal("replayed success")
	}
}

func TestBlockedRunRepairCannotReenterInstallation(t *testing.T) {
	run := resource.Resource{Metadata: resource.Metadata{UID: "run-uid"}, Status: map[string]any{"phase": "Blocked", "attemptID": "run-uid", "snapshot": map[string]any{}, "maintenance": true, "liveBootID": "live"}}
	next := cloneMap(run.Status)
	next["phase"] = "AwaitingInstalled"
	next["netbootArmed"] = false
	next["bootTarget"] = "installed"
	if err := validateProvisioningRunStatus(run, next); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"Installing", "AwaitingLive", "Succeeded"} {
		bad := cloneMap(next)
		bad["phase"] = phase
		if validateProvisioningRunStatus(run, bad) == nil {
			t.Fatal(phase)
		}
	}
}

func TestProvisioningRunGeneratedHTTPRoutesAndSafety(t *testing.T) {
	host, machine, run := runFixture()
	storage := testutil.NewStore(host, machine)
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	call := func(method, path, body, version string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if method == "PATCH" {
			r.Header.Set("Content-Type", "application/merge-patch+json")
		}
		if version != "" {
			r.Header.Set("If-Match", `"`+version+`"`)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	manifest := func(name string) string {
		body, _ := json.Marshal(map[string]any{"apiVersion": resource.APIVersion, "kind": "ProvisioningRun", "metadata": map[string]any{"name": name}, "spec": run.Spec})
		return string(body)
	}
	w := call("POST", "/api/v1alpha1/provisioning-runs", manifest("run"), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w = call("POST", "/api/v1alpha1/provisioning-runs", manifest("duplicate"), "")
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	w = call("GET", "/api/v1alpha1/provisioning-runs/run", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	w = call("PATCH", "/api/v1alpha1/provisioning-runs/run", `{"storage":{"disks":[{"deviceID":"serial:other","role":"system"}]}}`, "1")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("immutable: %d %s", w.Code, w.Body.String())
	}
	w = call("DELETE", "/api/v1alpha1/provisioning-runs/run", "", "1")
	if w.Code != http.StatusConflict {
		t.Fatalf("reserved delete: %d %s", w.Code, w.Body.String())
	}
	created, _ := storage.Get(context.Background(), "ProvisioningRun", "run")
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": created.Metadata.UID}, "status": map[string]any{"phase": "Succeeded", "maintenance": false}})
	w = call("PATCH", "/api/v1alpha1/provisioning-runs/run/status", string(body), "1")
	if w.Code != http.StatusConflict {
		t.Fatalf("false success: %d %s", w.Code, w.Body.String())
	}
}
