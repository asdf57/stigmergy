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

func TestArchNetbootIncludesUpstreamBootInterfaceSelection(t *testing.T) {
	w := httptest.NewRecorder()
	api := &Server{}
	api.renderISOBoot(w, "arch", []apigen.ISOArtifact{
		{Type: apigen.Kernel, Url: "https://files.example/kernel"},
		{Type: apigen.Initrd, Url: "https://files.example/initrd"},
		{Type: apigen.Rootfs, Url: "https://files.example/arch/x86_64/airootfs.sfs"},
	})
	for _, required := range []string{"ip=dhcp", "net.ifnames=0", "BOOTIF=01-${netX/mac}"} {
		if !strings.Contains(w.Body.String(), required) {
			t.Fatalf("missing %s in %s", required, w.Body.String())
		}
	}
}

func TestPinnedBootDoesNotFollowNewISOOrAuthorityBuild(t *testing.T) {
	resources := testutil.NewStore(
		resource.Resource{Kind: "ISO", Metadata: resource.Metadata{Name: "iso", UID: "iso-uid"}},
		resource.Resource{Kind: "SSHCertificateAuthority", Metadata: resource.Metadata{Name: "ca", UID: "ca-uid"}},
		resource.Resource{Kind: "SSHKeyPair", Metadata: resource.Metadata{Name: "key", UID: "key-uid"}},
	)
	host := registry.NewServer(resource.Metadata{Name: "host", UID: "host-uid"}, apigen.ServerSpec{})
	ref := apigen.ResourceReference{Name: "machine", Uid: "machine-uid"}
	snapshot := apigen.ProvisioningRunSnapshot{ServerUID: "host-uid", MachineRef: ref,
		IsoRef: apigen.ResourceReference{Name: "iso", Uid: "iso-uid"}, AuthorityRef: apigen.ResourceReference{Name: "ca", Uid: "ca-uid"},
		KeyPairRef: apigen.ResourceReference{Name: "key", Uid: "key-uid"}, Distribution: "debian", IsoBuildID: "old-build",
		Artifacts: []apigen.ISOArtifact{{Type: "kernel", Url: "https://files.example/old-build/kernel"}, {Type: "initrd", Url: "https://files.example/old-build/initrd"}, {Type: "rootfs", Url: "https://files.example/old-build/rootfs"}}}
	host.Status = &apigen.ServerStatus{MachineRef: &ref}
	api := &Server{store: resources}
	boot := func() string {
		w := httptest.NewRecorder()
		api.servePinnedBoot(w, httptest.NewRequest("GET", "/ipxe/test", nil), host, &snapshot)
		return w.Body.String()
	}
	if body := boot(); !strings.Contains(body, "old-build/kernel") || strings.Contains(body, "exit 1") {
		t.Fatal(body)
	}
	replaced := resources.Resources["ISO/iso"]
	replaced.Metadata.UID = "replacement"
	resources.Resources["ISO/iso"] = replaced
	if !strings.Contains(boot(), "exit 1") {
		t.Fatal("booted replacement identity")
	}
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

func TestIPXEBoundServerWithoutProvisioningDoesNotBootLive(t *testing.T) {
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
	storedServer.Status = map[string]any{"machineRef": map[string]any{"name": "machine-1", "uid": "machine-uid"}}
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
	want := "#!ipxe\necho No eligible live provisioning request\nsleep 10\nexit 1\n"
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

func TestBoundNewServerBootsConfiguredLiveImageWithoutDiskOrRun(t *testing.T) {
	image := resource.Resource{Kind: "ISO", Metadata: resource.Metadata{Name: "image", UID: "image-uid", Generation: 1}, Spec: map[string]any{"distribution": "debian", "sshCertificateAuthorityRef": map[string]any{"name": "ca"}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "desiredTrustBundleDigest": "digest", "authorityRef": map[string]any{"name": "ca", "uid": "ca-uid"}, "artifacts": []any{
		map[string]any{"type": "kernel", "url": "https://files.example/kernel"}, map[string]any{"type": "initrd", "url": "https://files.example/initrd"}, map[string]any{"type": "rootfs", "url": "https://files.example/rootfs"}}}}
	authority := resource.Resource{Kind: "SSHCertificateAuthority", Metadata: resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "trustBundleDigest": "digest"}}
	host := resource.Resource{Kind: "Server", Metadata: resource.Metadata{Name: "host", UID: "host-uid"}, Spec: map[string]any{"provisioning": map[string]any{"enabled": true}, "operatingSystem": map[string]any{"distribution": "debian"}, "boot": map[string]any{"isoRef": map[string]any{"name": "image"}}, "sshCertificateAuthorityRef": map[string]any{"name": "ca"}}, Status: map[string]any{"machineRef": map[string]any{"name": "machine", "uid": "machine-uid"}, "bootISORef": map[string]any{"name": "image", "uid": "image-uid"}, "sshTrust": map[string]any{"authorityRef": map[string]any{"name": "ca", "uid": "ca-uid"}, "publicBundle": "public"}}}
	machine := resource.Resource{Kind: "Machine", Metadata: resource.Metadata{Name: "machine", UID: "machine-uid"}, Status: map[string]any{"serverRef": map[string]any{"name": "host", "uid": "host-uid"}, "inventory": map[string]any{"interfaces": []any{map[string]any{"name": "eth0", "mac": "00:11:22:33:44:55"}}}}}
	storage := testutil.NewStore(image, authority, host, machine)
	for key, value := range storage.Resources {
		value.APIVersion = resource.APIVersion
		storage.Resources[key] = value
	}
	api := &Server{store: storage, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	boot := func() string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/ipxe/00:11:22:33:44:55", nil)
		r.SetPathValue("mac", "00:11:22:33:44:55")
		api.getIPXEBoot(w, r)
		return w.Body.String()
	}
	if body := boot(); !strings.Contains(body, "kernel https://files.example/kernel") || strings.Contains(body, "exit 1") {
		t.Fatal(body)
	}
	host.Status["provisioning"] = map[string]any{"provisioned": true}
	host.APIVersion = resource.APIVersion
	storage.Resources["Server/host"] = host
	if !strings.Contains(boot(), "exit 1") {
		t.Fatal("installed Server unexpectedly booted live without a run")
	}
}
