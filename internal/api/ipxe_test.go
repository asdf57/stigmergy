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
	"github.com/asdf57/stigmergy/internal/testutil"
)

type ipxeStore struct {
	fakeStore
	machine resource.Resource
	server  resource.Resource
}

func TestISOBootRefusesStaleTrustAndReplacedIdentities(t *testing.T) {
	image := resource.Resource{APIVersion: resource.APIVersion, Kind: "ISO", Metadata: resource.Metadata{Name: "image", UID: "image-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{"distribution": "debian", "sshCertificateAuthorityRef": map[string]any{"name": "ca"}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "desiredTrustBundleDigest": "digest", "authorityRef": map[string]any{"name": "ca", "uid": "ca-uid"}, "artifacts": []any{map[string]any{"type": "kernel", "url": "https://files.example/vmlinuz", "sha256": strings.Repeat("a", 64)}, map[string]any{"type": "initrd", "url": "https://files.example/initrd.img", "sha256": strings.Repeat("a", 64)}, map[string]any{"type": "rootfs", "url": "https://files.example/rootfs", "sha256": strings.Repeat("a", 64)}}}}
	authority := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHCertificateAuthority", Metadata: resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1}, Spec: map[string]any{}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "trustBundleDigest": "digest"}}
	s := testutil.NewStore(image, authority)
	host := registry.NewServer(resource.Metadata{Name: "host"}, apigen.ServerSpec{Boot: &apigen.ServerBootSpec{IsoRef: apigen.ServerDependencyReference{Name: "image"}}, SshCertificateAuthorityRef: &apigen.ServerDependencyReference{Name: "ca"}})
	host.Status = &apigen.ServerStatus{BootISORef: &apigen.ResourceReference{Name: "image", Uid: "image-uid"}, SshTrust: &apigen.ServerSSHTrustStatus{AuthorityRef: apigen.ResourceReference{Name: "ca", Uid: "ca-uid"}}}
	api := &Server{store: s}
	boot := func() string {
		w := httptest.NewRecorder()
		api.serveISOBoot(w, httptest.NewRequest("GET", "/ipxe/test", nil), host)
		return w.Body.String()
	}
	if body := boot(); !strings.Contains(body, "kernel https://files.example/vmlinuz boot=live") || strings.Contains(body, "exit 1") {
		t.Fatal(body)
	}
	authority.Status["trustBundleDigest"] = "rotated"
	s.Resources["SSHCertificateAuthority/ca"] = authority
	if !strings.Contains(boot(), "exit 1") {
		t.Fatal("stale ISO trust booted")
	}
	authority.Status["trustBundleDigest"] = "digest"
	authority.Metadata.UID = "replacement"
	s.Resources["SSHCertificateAuthority/ca"] = authority
	if !strings.Contains(boot(), "exit 1") {
		t.Fatal("replaced authority booted")
	}
	authority.Metadata.UID = "ca-uid"
	s.Resources["SSHCertificateAuthority/ca"] = authority
	image.Metadata.UID = "replacement"
	s.Resources["ISO/image"] = image
	if !strings.Contains(boot(), "exit 1") {
		t.Fatal("replaced ISO booted")
	}
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
			Architecture: apigen.ServerOperatingSystemArchitectureAmd64, BootMode: apigen.ServerOperatingSystemBootModeUefi,
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
