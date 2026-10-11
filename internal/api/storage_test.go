package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestStoragePlanPinsSpecificationsAndSupportsDifferentServers(t *testing.T) {
	for _, name := range []string{"alpha", "beta"} {
		t.Run(name, func(t *testing.T) {
			host, machine, run := runFixture()
			host.Metadata.Name = name
			object(run.Spec["serverRef"])["name"] = name
			object(machine.Status["serverRef"])["name"] = name
			storage := newRunStore(host, machine)
			root := storage.Resources["DiskPartition/host-root"]
			root.Spec["number"] = 7
			if name == "beta" {
				root.Spec["capacity"] = map[string]any{"storage": "3Gi"}
			}
			storage.Resources["DiskPartition/host-root"] = root
			api := &Server{store: storage}
			created, err := api.createProvisioningRun(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			entries := object(created.Status["storagePlan"])["partitions"].([]any)
			pinned := object(entries[1])
			if object(pinned["spec"])["number"] != float64(7) {
				t.Fatal("partition number was not pinned")
			}
			if name == "beta" && pinned["endMiB"] != float64(513+3072) {
				t.Fatal("fixed sizing ignored")
			}
			root.Spec["number"] = 8
			if object(pinned["spec"])["number"] != float64(7) {
				t.Fatal("resource edit changed pinned plan")
			}
			next := cloneMap(created.Status)
			object(next["storagePlan"])["partitions"] = []any{}
			if validateProvisioningRunStatus(created, next) == nil {
				t.Fatal("pinned plan can be rewritten")
			}
		})
	}
}

func TestStoragePlanRejectsUnsafeOrStaleLayouts(t *testing.T) {
	cases := map[string]func(*testutil.Store, *resource.Resource){
		"missing refs": func(_ *testutil.Store, run *resource.Resource) { delete(object(run.Spec["storage"]), "partitionRefs") },
		"stale generation": func(s *testutil.Store, _ *resource.Resource) {
			partition := s.Resources["DiskPartition/host-root"]
			partition.Metadata.Generation++
			s.Resources["DiskPartition/host-root"] = partition
		},
		"replaced resource": func(s *testutil.Store, _ *resource.Resource) {
			partition := s.Resources["DiskPartition/host-root"]
			partition.Metadata.UID = "replacement"
			s.Resources["DiskPartition/host-root"] = partition
		},
		"wrong server": func(s *testutil.Store, _ *resource.Resource) {
			object(s.Resources["DiskPartition/host-root"].Spec["serverRef"])["name"] = "another"
		},
		"wrong disk": func(s *testutil.Store, _ *resource.Resource) {
			object(s.Resources["DiskPartition/host-root"].Spec["diskSelector"])["serial"] = "other"
		},
		"conflicting selectors": func(s *testutil.Store, _ *resource.Resource) {
			object(s.Resources["DiskPartition/host-root"].Spec["diskSelector"])["wwn"] = "other"
		},
		"ambiguous selector": func(s *testutil.Store, _ *resource.Resource) {
			machine := s.Resources["Machine/machine"]
			inventory := object(machine.Status["inventory"])
			disks := inventory["storage"].([]any)
			inventory["storage"] = append(disks, cloneMap(object(disks[0])))
		},
		"duplicate number": func(s *testutil.Store, _ *resource.Resource) {
			s.Resources["DiskPartition/host-root"].Spec["number"] = 1
		},
		"oversized root": func(s *testutil.Store, _ *resource.Resource) {
			s.Resources["DiskPartition/host-root"].Spec["capacity"] = map[string]any{"storage": "100Ti"}
		},
		"remaining not last": func(s *testutil.Store, _ *resource.Resource) {
			s.Resources["DiskPartition/host-efi"].Spec["capacity"] = map[string]any{"useRemaining": true}
		},
		"reserved mount": func(s *testutil.Store, _ *resource.Resource) {
			partition := s.Resources["DiskPartition/host-root"]
			partition.Spec["role"] = "data"
			partition.Spec["mountPoint"] = "/proc/data"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			host, machine, run := runFixture()
			storage := newRunStore(host, machine)
			mutate(storage, &run)
			api := &Server{store: storage}
			if _, err := api.createProvisioningRun(context.Background(), run); err == nil {
				t.Fatal("unsafe plan accepted")
			}
			if _, err := storage.Get(context.Background(), "ProvisioningRun", "run"); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("orphan run")
			}
			current, _ := storage.Get(context.Background(), "Server", host.Metadata.Name)
			if activeProvisioning(current.Status) {
				t.Fatal("rejected plan reserved server")
			}
		})
	}
}

func TestStoragePlanSupportsSwapAndDataMounts(t *testing.T) {
	host, machine, run := runFixture()
	storage := newRunStore(host, machine)
	root := storage.Resources["DiskPartition/host-root"]
	root.Spec["capacity"] = map[string]any{"storage": "3Gi"}
	storage.Resources["DiskPartition/host-root"] = root
	for _, role := range []string{"swap", "data"} {
		partition := root
		partition.Spec = cloneMap(root.Spec)
		partition.Metadata.Name = "host-" + role
		partition.Metadata.UID = role + "-uid"
		partition.Spec["role"] = role
		if role == "swap" {
			partition.Spec["number"] = 4
			partition.Spec["capacity"] = map[string]any{"storage": "1Gi"}
			partition.Spec["filesystem"] = "swap"
			delete(partition.Spec, "mountPoint")
		} else {
			partition.Spec["number"] = 7
			partition.Spec["capacity"] = map[string]any{"useRemaining": true}
			partition.Spec["mountPoint"] = "/var/lib"
		}
		storage.Resources["DiskPartition/"+partition.Metadata.Name] = partition
		requestStorage := object(run.Spec["storage"])
		requestStorage["partitionRefs"] = append(requestStorage["partitionRefs"].([]any), map[string]any{
			"name": partition.Metadata.Name, "uid": partition.Metadata.UID, "generation": 1,
		})
	}
	api := &Server{store: storage}
	created, err := api.createProvisioningRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	entries := object(created.Status["storagePlan"])["partitions"].([]any)
	if len(entries) != 4 || object(object(entries[2])["spec"])["role"] != "swap" || object(object(entries[3])["spec"])["mountPoint"] != "/var/lib" {
		t.Fatal("swap and data layout was not preserved")
	}
}

func TestDiskPartitionRoutesValidateAllSpecWritesWithoutProvisioning(t *testing.T) {
	host, machine, _ := runFixture()
	storage := newRunStore(host, machine)
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	partition := storage.Resources["DiskPartition/host-root"]
	partition.APIVersion = resource.APIVersion
	partition.Metadata.Name = "new-root"
	for _, method := range []string{"POST", "PUT"} {
		endpoint := "/api/v1alpha1/disk-partitions"
		if method == "PUT" {
			endpoint += "/new-root"
		}
		partition.Spec["mountPoint"] = "/wrong"
		data, _ := json.Marshal(map[string]any{"apiVersion": resource.APIVersion, "kind": "DiskPartition", "metadata": map[string]any{"name": "new-root"}, "spec": partition.Spec})
		r := httptest.NewRequest(method, endpoint, strings.NewReader(string(data)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 422 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body.String())
		}
	}
	if storage.Writes != 0 {
		t.Fatal("invalid partition caused writes")
	}
}

func TestCapacityQuantitiesRejectOverflowAndAmbiguity(t *testing.T) {
	for _, bad := range []string{"0Mi", "1.5Gi", "1G", "999999999999999999Ti", "-1Gi"} {
		if _, err := capacityMiB(bad); err == nil {
			t.Fatal(bad)
		}
	}
}
