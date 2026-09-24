package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

type ipxeStore struct {
	fakeStore
	machine resource.Resource
	server  resource.Resource
}

func (s *ipxeStore) List(_ context.Context, kind string) (resource.List, error) {
	items := []resource.Resource{}
	if kind == registry.MachineResource.Kind {
		items = append(items, s.machine)
	}
	return resource.List{APIVersion: resource.APIVersion, Kind: kind + "List", Items: items}, nil
}

func (s *ipxeStore) Get(_ context.Context, kind, name string) (resource.Resource, error) {
	if kind == registry.ServerResource.Kind && name == s.server.Metadata.Name {
		return s.server, nil
	}
	return resource.Resource{}, store.ErrNotFound
}

func TestIPXEResolvesServerBoundByPortLocation(t *testing.T) {
	t.Parallel()

	server := registry.NewServer(resource.Metadata{Name: "desktop", UID: "server-uid"}, apigen.ServerSpec{
		MachineSelector: apigen.ServerMachineSelector{Location: apigen.MachineLocation{LldpPort: "ether3", SwitchMac: "00:11:22:33:44:55"}},
		OperatingSystem: &apigen.ServerOperatingSystem{
			Distribution: "debian", Version: "trixie",
			Architecture: apigen.Amd64, BootMode: apigen.Uefi,
		},
	})
	storedServer, err := server.Encode()
	if err != nil {
		t.Fatal(err)
	}
	machine := registry.NewMachine(resource.Metadata{Name: "machine-1", UID: "machine-uid"}, apigen.MachineSpec{
		Location: apigen.MachineLocation{LldpPort: "ether3", SwitchMac: "00:11:22:33:44:55"},
	})
	machine.Status = &apigen.MachineStatus{
		ServerRef: &apigen.ResourceReference{Name: "desktop", Uid: "server-uid"},
		Inventory: &apigen.MachineReportSpec{Interfaces: []apigen.MachineReportNetworkInterface{{
			Name: "eth0", Mac: "aa:bb:cc:dd:ee:ff", Addresses: []apigen.MachineReportNetworkAddress{},
		}}},
	}
	storedMachine, err := machine.Encode()
	if err != nil {
		t.Fatal(err)
	}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &ipxeStore{machine: storedMachine, server: storedServer}, time.Second)

	request := httptest.NewRequest(http.MethodGet, "/ipxe/AA:BB:CC:DD:EE:FF", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	want := "#!ipxe\necho Booting desktop\nchain /debian_boot.ipxe\n"
	if response.Body.String() != want {
		t.Fatalf("body = %q, want %q", response.Body.String(), want)
	}
}

func TestIPXEUnknownMACReturnsBootableErrorScript(t *testing.T) {
	t.Parallel()

	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeStore{}, time.Second)
	request := httptest.NewRequest(http.MethodGet, "/ipxe/aa:bb:cc:dd:ee:ff", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if body := response.Body.String(); !strings.Contains(body, "No discovered Machine matches MAC") || !strings.Contains(body, "exit 1") {
		t.Fatalf("body = %q", body)
	}
}
