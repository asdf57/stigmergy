// server_test.go verifies generated-schema validation and the shared resource
// lifecycle through the public HTTP API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	created         resource.Resource
	updateCalls     int
	updateConflicts int
	conflictSpec    map[string]any
}

func (f *fakeStore) Create(_ context.Context, value resource.Resource) (resource.Resource, error) {
	value.Metadata.UID = "test-uid"
	value.Metadata.Generation = 1
	value.Metadata.ResourceVersion = "7"
	value.Metadata.CreationTimestamp = time.Unix(1, 0).UTC()
	f.created = value
	return value, nil
}

func (f *fakeStore) Get(_ context.Context, _, _ string) (resource.Resource, error) {
	if f.created.Kind == "" {
		return resource.Resource{}, store.ErrNotFound
	}
	return f.created, nil
}

func (f *fakeStore) List(_ context.Context, kind string) (resource.List, error) {
	items := []resource.Resource{}
	if f.created.Kind != "" {
		items = append(items, f.created)
	}
	return resource.List{
		APIVersion: resource.APIVersion,
		Kind:       kind + "List",
		Metadata:   resource.ListMetadata{ResourceVersion: "7"},
		Items:      items,
	}, nil
}

func (f *fakeStore) Update(_ context.Context, value resource.Resource, expectedRevision int64) (resource.Resource, error) {
	f.updateCalls++
	if f.updateConflicts > 0 {
		f.updateConflicts--
		current, _ := strconv.ParseInt(f.created.Metadata.ResourceVersion, 10, 64)
		f.created.Metadata.ResourceVersion = strconv.FormatInt(current+1, 10)
		f.created.Status = map[string]any{"phase": "ConcurrentUpdate"}
		if f.conflictSpec != nil {
			f.created.Spec = f.conflictSpec
		}
		return resource.Resource{}, store.ErrConflict
	}
	if f.created.Metadata.ResourceVersion != strconv.FormatInt(expectedRevision, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	value.Metadata.Generation = f.created.Metadata.Generation
	if !reflect.DeepEqual(value.Spec, f.created.Spec) {
		value.Metadata.Generation++
	}
	value.Status = f.created.Status
	value.Metadata.ResourceVersion = strconv.FormatInt(expectedRevision+1, 10)
	f.created = value
	return value, nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, kind, name string, status map[string]any, _ int64) (resource.Resource, error) {
	f.created.Status = status
	f.created.Metadata.ResourceVersion = "8"
	return f.created, nil
}

func (f *fakeStore) Delete(_ context.Context, _, _ string, _ int64) error { return nil }
func (f *fakeStore) DeleteCollection(_ context.Context, _ string) (int64, error) {
	if f.created.Kind == "" {
		return 0, nil
	}
	f.created = resource.Resource{}
	return 1, nil
}
func (f *fakeStore) Ready(_ context.Context) error { return nil }

func TestCreateAndFetchMachineReport(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body, err := json.Marshal(apigen.MachineReportCreate{
		ApiVersion: apigen.MachineReportCreateApiVersionHomelabIov1alpha1,
		Kind:       apigen.MachineReportCreateKindMachineReport,
		Metadata:   apigen.Metadata{Name: "lab-node"},
		Spec:       testMachineReportSpec(),
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/machine-reports", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("ETag"); got != `"7"` {
		t.Fatalf("ETag = %q", got)
	}
	var created apigen.MachineReport
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Metadata.Uid == nil || *created.Metadata.Uid != "test-uid" {
		t.Fatalf("UID = %v", created.Metadata.Uid)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1alpha1/machine-reports/lab-node", nil)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/v1alpha1/machine-reports", nil)
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listResponse.Code, listResponse.Body.String())
	}
	var list apigen.MachineReportList
	if err := json.NewDecoder(listResponse.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name != "lab-node" {
		t.Fatalf("list items = %#v", list.Items)
	}
}

func TestMachineReportSchemaValidation(t *testing.T) {
	t.Parallel()

	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeStore{}, time.Second)
	request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/machine-reports/lab-node", bytes.NewReader([]byte(`{"storage":[]}`)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func TestCreateMachineWithDeclaredLocation(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body, err := json.Marshal(apigen.MachineCreate{
		ApiVersion: apigen.MachineCreateApiVersionHomelabIov1alpha1,
		Kind:       apigen.MachineCreateKindMachine,
		Metadata:   apigen.Metadata{Name: "lab-node"},
		Spec: apigen.MachineSpec{
			Location: apigen.MachineLocation{LldpPort: "Ethernet1", SwitchMac: "00:11:22:33:44:55"},
		},
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/machines", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var created apigen.Machine
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Spec.Location.LldpPort != "Ethernet1" || created.Spec.Location.SwitchMac != "00:11:22:33:44:55" {
		t.Fatalf("Machine location = %#v", created.Spec.Location)
	}
	if created.Status != nil {
		t.Fatalf("client-created Machine status = %#v, want nil", created.Status)
	}
}

func TestCreateMachineFromYAMLManifest(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body := `apiVersion: homelab.io/v1alpha1
kind: Machine
metadata:
  name: desktop
spec:
  location:
    lldp_port: bridge/ether3
    switch_mac: d4:01:c3:27:91:67
`
	request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/machines", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/yaml")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var created apigen.Machine
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Metadata.Name != "desktop" || created.Spec.Location.LldpPort != "bridge/ether3" || created.Spec.Location.SwitchMac != "d4:01:c3:27:91:67" {
		t.Fatalf("created Machine = %#v", created)
	}
}

func TestCreateInventoryCaptureGroupFromYAML(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body := `apiVersion: homelab.io/v1alpha1
kind: InventoryCaptureGroup
metadata:
  name: servers
spec:
  selector:
    matchKinds:
      - apiVersion: homelab.io/v1alpha1
        kind: Server
    matchLabels:
      homelab.io/type: server
  groups:
    - name: workstations
      selector:
        matchLabels:
          homelab.io/role: workstation
  groupVars:
    all:
      ansible_user: matt
    workstations:
      desktop_environment: true
`
	request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/inventory-capture-groups", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/yaml")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var created apigen.InventoryCaptureGroup
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Metadata.Name != "servers" || created.Spec.Selector.MatchKinds == nil || len(*created.Spec.Selector.MatchKinds) != 1 || (*created.Spec.Selector.MatchKinds)[0].Kind != "Server" || created.Spec.Selector.MatchLabels == nil || (*created.Spec.Selector.MatchLabels)["homelab.io/type"] != "server" {
		t.Fatalf("created InventoryCaptureGroup = %#v", created)
	}
	if created.Spec.Groups == nil || len(*created.Spec.Groups) != 1 || (*created.Spec.Groups)[0].Name != "workstations" || created.Spec.GroupVars == nil || (*created.Spec.GroupVars)["all"]["ansible_user"] != "matt" {
		t.Fatalf("created InventoryCaptureGroup groups and vars = %#v", created.Spec)
	}
}

func TestCreateInventoryPublicationResourcesFromYAML(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "GitRepository",
			path: "/api/v1alpha1/git-repositories",
			body: `apiVersion: homelab.io/v1alpha1
kind: GitRepository
metadata:
  name: ansible-inventory
spec:
  url: https://github.com/example/inventory.git
  branch: main
  authentication:
    username: x-access-token
    passwordEnvironmentVariable: GITHUB_TOKEN
`,
		},
		{
			name: "InventoryPublication",
			path: "/api/v1alpha1/inventory-publications",
			body: `apiVersion: homelab.io/v1alpha1
kind: InventoryPublication
metadata:
  name: servers-to-git
spec:
  inventoryCaptureGroupRef:
    name: servers
  destinationRef:
    apiVersion: homelab.io/v1alpha1
    kind: GitRepository
    name: ansible-inventory
  target:
    branch: servers-inventory
    rootPath: inventories/servers
    layout: ansible-directory
    inventoryFile: inventory.yaml
`,
		},
		{
			name: "SecretStore",
			path: "/api/v1alpha1/secret-stores",
			body: `apiVersion: homelab.io/v1alpha1
kind: SecretStore
metadata:
  name: openbao
spec:
  provider:
    openBao:
      address: http://openbao:8200
      kvV2Mount: kv2
      keyPrefix: secrets
      authentication:
        tokenFile: /run/openbao/token
`,
		},
		{
			name: "SSHKeyPair",
			path: "/api/v1alpha1/ssh-key-pairs",
			body: `apiVersion: homelab.io/v1alpha1
kind: SSHKeyPair
metadata:
  name: ansible-homelab
spec:
  algorithm: ed25519
  secretStoreRef:
    name: openbao
  path: automation/ansible-homelab
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/yaml")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var created map[string]any
			if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if created["kind"] != test.name {
				t.Fatalf("created kind = %v, want %s", created["kind"], test.name)
			}
			if test.name == "SSHKeyPair" {
				metadata := created["metadata"].(map[string]any)
				finalizers := metadata["finalizers"].([]any)
				if len(finalizers) != 1 || finalizers[0] != "homelab.io/ssh-key-pair-cleanup" {
					t.Fatalf("%s finalizers = %#v", test.name, finalizers)
				}
			}
		})
	}
}

func TestDeleteFinalizedResourceReturnsAccepted(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{created: resource.Resource{
		APIVersion: registry.SSHKeyPairResource.APIVersion,
		Kind:       registry.SSHKeyPairResource.Kind,
		Metadata: resource.Metadata{
			Name:            "ansible-mgmt",
			ResourceVersion: "7",
			Finalizers:      []string{"homelab.io/ssh-key-pair-cleanup"},
		},
		Spec: map[string]any{},
	}}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	request := httptest.NewRequest(http.MethodDelete, "/api/v1alpha1/ssh-key-pairs/ansible-mgmt", nil)
	request.Header.Set("If-Match", `"7"`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusAccepted, response.Body.String())
	}
}

func TestPutServerFromYAMLSpec(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body := `apiVersion: homelab.io/v1alpha1
kind: Server
metadata:
  name: desktop
spec:
  machineSelector:
    location:
      lldp_port: bridge/ether3
      switch_mac: d4:01:c3:27:91:67
  hostName: atlas
`
	request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/servers/desktop", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-yaml")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var created apigen.Server
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Spec.HostName == nil || *created.Spec.HostName != "atlas" {
		t.Fatalf("created Server hostName = %#v", created.Spec.HostName)
	}
}

func TestPutServerReplacesDesiredStateAndPreservesOwnedState(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{created: resource.Resource{
		APIVersion: registry.ServerResource.APIVersion,
		Kind:       registry.ServerResource.Kind,
		Metadata: resource.Metadata{
			Name:              "desktop",
			UID:               "existing-uid",
			ResourceVersion:   "7",
			Generation:        3,
			CreationTimestamp: time.Unix(1, 0).UTC(),
			Finalizers:        []string{"controller.example/cleanup"},
			Labels:            map[string]string{"old": "label"},
			Annotations:       map[string]string{"old": "annotation"},
		},
		Spec: map[string]any{"machineSelector": map[string]any{"location": map[string]any{
			"lldp_port": "old-port", "switch_mac": "00:11:22:33:44:55",
		}}},
		Status: map[string]any{"phase": "Ready"},
	}}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body := `apiVersion: homelab.io/v1alpha1
kind: Server
metadata:
  name: desktop
  labels:
    homelab.io/role: workstation
  annotations:
    homelab.io/source: homelab-init
spec:
  machineSelector:
    location:
      lldp_port: bridge/ether3
      switch_mac: d4:01:c3:27:91:67
`

	put := func(ifMatch string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/servers/desktop", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/yaml")
		if ifMatch != "" {
			request.Header.Set("If-Match", ifMatch)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := put("")
	if response.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body = %s", response.Code, response.Body.String())
	}
	if storage.created.Metadata.UID != "existing-uid" ||
		!storage.created.Metadata.CreationTimestamp.Equal(time.Unix(1, 0).UTC()) ||
		!reflect.DeepEqual(storage.created.Metadata.Finalizers, []string{"controller.example/cleanup"}) ||
		storage.created.Status["phase"] != "Ready" {
		t.Fatalf("PUT changed server/controller-owned state: %#v", storage.created)
	}
	if !reflect.DeepEqual(storage.created.Metadata.Labels, map[string]string{"homelab.io/role": "workstation"}) ||
		!reflect.DeepEqual(storage.created.Metadata.Annotations, map[string]string{"homelab.io/source": "homelab-init"}) {
		t.Fatalf("PUT metadata = %#v", storage.created.Metadata)
	}

	response = put(`"8"`)
	if response.Code != http.StatusOK || response.Header().Get("ETag") != `"8"` {
		t.Fatalf("no-op status = %d, ETag = %q, body = %s", response.Code, response.Header().Get("ETag"), response.Body.String())
	}
	if storage.updateCalls != 1 {
		t.Fatalf("Update calls = %d, want 1 after no-op PUT", storage.updateCalls)
	}
}

func TestPutServerRetriesUnconditionalConflictButHonorsIfMatch(t *testing.T) {
	t.Parallel()

	newStorage := func() *fakeStore {
		return &fakeStore{created: resource.Resource{
			APIVersion: registry.ServerResource.APIVersion,
			Kind:       registry.ServerResource.Kind,
			Metadata:   resource.Metadata{Name: "desktop", ResourceVersion: "7", Generation: 1},
			Spec: map[string]any{"machineSelector": map[string]any{"location": map[string]any{
				"lldp_port": "old-port", "switch_mac": "00:11:22:33:44:55",
			}}},
		}}
	}
	body := `{"apiVersion":"homelab.io/v1alpha1","kind":"Server","metadata":{"name":"desktop"},"spec":{"machineSelector":{"location":{"lldp_port":"new-port","switch_mac":"00:11:22:33:44:55"}}}}`

	storage := newStorage()
	storage.updateConflicts = 1
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/servers/desktop", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || storage.updateCalls != 2 || storage.created.Status["phase"] != "ConcurrentUpdate" {
		t.Fatalf("unconditional PUT status = %d, calls = %d, resource = %#v, body = %s", response.Code, storage.updateCalls, storage.created, response.Body.String())
	}

	storage = newStorage()
	storage.updateConflicts = 1
	concurrentSpec := map[string]any{"machineSelector": map[string]any{"location": map[string]any{
		"lldp_port": "concurrent-port", "switch_mac": "00:11:22:33:44:55",
	}}}
	storage.conflictSpec = concurrentSpec
	handler = New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	request = httptest.NewRequest(http.MethodPut, "/api/v1alpha1/servers/desktop", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || storage.updateCalls != 1 {
		t.Fatalf("concurrent desired-state PUT status = %d, calls = %d, body = %s", response.Code, storage.updateCalls, response.Body.String())
	}

	storage = newStorage()
	handler = New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	request = httptest.NewRequest(http.MethodPut, "/api/v1alpha1/servers/desktop", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"6"`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || storage.updateCalls != 0 {
		t.Fatalf("conditional PUT status = %d, calls = %d, body = %s", response.Code, storage.updateCalls, response.Body.String())
	}
}

func TestPutServerRequiresMatchingManifestIdentity(t *testing.T) {
	t.Parallel()

	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeStore{}, time.Second)
	tests := []struct {
		name string
		body string
	}{
		{name: "apiVersion", body: `{"apiVersion":"other.io/v1","kind":"Server","metadata":{"name":"desktop"},"spec":{"machineSelector":{"location":{"lldp_port":"port","switch_mac":"00:11:22:33:44:55"}}}}`},
		{name: "kind", body: `{"apiVersion":"homelab.io/v1alpha1","kind":"Machine","metadata":{"name":"desktop"},"spec":{"machineSelector":{"location":{"lldp_port":"port","switch_mac":"00:11:22:33:44:55"}}}}`},
		{name: "name", body: `{"apiVersion":"homelab.io/v1alpha1","kind":"Server","metadata":{"name":"other"},"spec":{"machineSelector":{"location":{"lldp_port":"port","switch_mac":"00:11:22:33:44:55"}}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/servers/desktop", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest && response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestYAMLRequestsRejectInvalidAndMultipleDocuments(t *testing.T) {
	t.Parallel()

	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeStore{}, time.Second)
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid", body: "metadata: ["},
		{name: "multiple documents", body: "apiVersion: homelab.io/v1alpha1\n---\nkind: Machine\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/machines", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/yaml")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
		})
	}
}

func TestCreateServerWithDesiredHostConfiguration(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body := []byte(`{
  "apiVersion": "homelab.io/v1alpha1",
  "kind": "Server",
  "metadata": {"name": "server-01"},
  "spec": {
    "machineSelector": {"location": {"lldp_port": "Ethernet1", "switch_mac": "00:11:22:33:44:55"}},
    "hostName": "atlas",
    "domainName": "homelab.local",
    "operatingSystem": {
      "distribution": "debian",
      "version": "13",
      "architecture": "amd64",
      "bootMode": "uefi",
      "installationSource": {
        "url": "https://images.homelab.local/debian-13-amd64.tar.zst",
        "checksum": {"algorithm": "sha256", "value": "abc123"}
      },
      "timezone": "America/New_York",
      "locale": "en_US.UTF-8",
      "kernelArguments": ["iommu=pt"]
    },
    "users": [{
      "name": "matt",
      "groups": ["sudo"],
      "shell": "/bin/bash",
      "locked": true
    }],
    "groups": ["all_servers", "storage_nodes"],
    "packages": ["curl", "vim"],
    "sysctls": {"net.ipv4.ip_forward": "1"},
    "featureFlags": {
      "daemon": {"enabled": true},
      "monitoring": {"enabled": true},
      "backup": {"enabled": false},
      "kubernetes": {"enabled": false, "role": "worker"}
    },
    "networking": {
      "management": {
        "interfaceSelector": {"attachedAtMachineLocation": true},
        "addressSelector": {"family": "ipv4", "subnet": "10.1.1.0/24"}
      }
    },
    "provisioning": {"enabled": true},
    "reconciliation": {"paused": false}
  }
}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/servers", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var created apigen.Server
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Spec.HostName == nil || *created.Spec.HostName != "atlas" {
		t.Fatalf("Server hostName = %#v", created.Spec.HostName)
	}
	if created.Spec.OperatingSystem == nil || created.Spec.OperatingSystem.Distribution != "debian" {
		t.Fatalf("Server operatingSystem = %#v", created.Spec.OperatingSystem)
	}
	if created.Spec.Provisioning == nil || !created.Spec.Provisioning.Enabled {
		t.Fatalf("Server provisioning = %#v", created.Spec.Provisioning)
	}
	if created.Spec.Networking == nil || created.Spec.Networking.Management == nil || created.Spec.Networking.Management.AddressSelector.Subnet != "10.1.1.0/24" {
		t.Fatalf("Server networking = %#v", created.Spec.Networking)
	}
}

func TestPatchServerMergesObjectsAndReplacesArrays(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	createBody := []byte(`{
  "apiVersion":"homelab.io/v1alpha1",
  "kind":"Server",
  "metadata":{"name":"server-01"},
  "spec":{
    "machineSelector":{"location":{"lldp_port":"Ethernet1","switch_mac":"00:11:22:33:44:55"}},
    "domainName":"homelab.local",
    "users":[{"name":"matt"}],
    "packages":["curl","vim"],
    "featureFlags":{"daemon":{"enabled":true},"backup":{"enabled":false}}
  }
}`)
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/servers", bytes.NewReader(createBody))
	createRequest.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createResponse.Code, createResponse.Body.String())
	}
	storage.created.Status = map[string]any{"phase": "Ready"}

	patchBody := []byte(`{
  "users":[{"name":"deploy"}],
  "packages":["jq"],
  "featureFlags":{"backup":{"enabled":true}}
}`)
	patchRequest := httptest.NewRequest(http.MethodPatch, "/api/v1alpha1/servers/server-01", bytes.NewReader(patchBody))
	patchRequest.Header.Set("Content-Type", "application/merge-patch+json")
	patchRequest.Header.Set("If-Match", `"7"`)
	patchResponse := httptest.NewRecorder()
	handler.ServeHTTP(patchResponse, patchRequest)

	if patchResponse.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", patchResponse.Code, patchResponse.Body.String())
	}
	if got := patchResponse.Header().Get("ETag"); got != `"8"` {
		t.Fatalf("patch ETag = %q, want %q", got, `"8"`)
	}
	var patched apigen.Server
	if err := json.NewDecoder(patchResponse.Body).Decode(&patched); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if patched.Spec.DomainName == nil || *patched.Spec.DomainName != "homelab.local" {
		t.Fatalf("patch lost unmentioned domainName: %#v", patched.Spec.DomainName)
	}
	if patched.Spec.FeatureFlags == nil || patched.Spec.FeatureFlags.Daemon == nil || !patched.Spec.FeatureFlags.Daemon.Enabled || patched.Spec.FeatureFlags.Backup == nil || !patched.Spec.FeatureFlags.Backup.Enabled {
		t.Fatalf("featureFlags were not recursively merged: %#v", patched.Spec.FeatureFlags)
	}
	if patched.Spec.Users == nil || len(*patched.Spec.Users) != 1 || (*patched.Spec.Users)[0].Name != "deploy" {
		t.Fatalf("users array = %#v, want atomic replacement", patched.Spec.Users)
	}
	if patched.Spec.Packages == nil || len(*patched.Spec.Packages) != 1 || (*patched.Spec.Packages)[0] != "jq" {
		t.Fatalf("packages array = %#v, want atomic replacement", patched.Spec.Packages)
	}
	if patched.Metadata.Generation == nil || *patched.Metadata.Generation != 2 {
		t.Fatalf("generation = %#v, want 2", patched.Metadata.Generation)
	}
	if patched.Status == nil || patched.Status.Phase == nil || *patched.Status.Phase != "Ready" {
		t.Fatalf("patch did not preserve status: %#v", patched.Status)
	}
}

func TestPatchServerRequiresCurrentETagAndValidMergedSpec(t *testing.T) {
	t.Parallel()

	newHandler := func() http.Handler {
		storage := &fakeStore{created: resource.Resource{
			APIVersion: registry.ServerResource.APIVersion,
			Kind:       registry.ServerResource.Kind,
			Metadata:   resource.Metadata{Name: "server-01", ResourceVersion: "7"},
			Spec: map[string]any{"machineSelector": map[string]any{"location": map[string]any{
				"lldp_port": "Ethernet1", "switch_mac": "00:11:22:33:44:55",
			}}},
		}}
		return New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	}

	tests := []struct {
		name    string
		ifMatch string
		patch   string
		want    int
	}{
		{name: "missing If-Match", patch: `{"hostName":"atlas"}`, want: http.StatusPreconditionRequired},
		{name: "stale If-Match", ifMatch: `"6"`, patch: `{"hostName":"atlas"}`, want: http.StatusConflict},
		{name: "remove required selector", ifMatch: `"7"`, patch: `{"machineSelector":null}`, want: http.StatusUnprocessableEntity},
		{name: "unknown field", ifMatch: `"7"`, patch: `{"unknown":true}`, want: http.StatusUnprocessableEntity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPatch, "/api/v1alpha1/servers/server-01", strings.NewReader(test.patch))
			request.Header.Set("Content-Type", "application/merge-patch+json")
			if test.ifMatch != "" {
				request.Header.Set("If-Match", test.ifMatch)
			}
			response := httptest.NewRecorder()
			newHandler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestPatchServerFromYAML(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{created: resource.Resource{
		APIVersion: registry.ServerResource.APIVersion,
		Kind:       registry.ServerResource.Kind,
		Metadata:   resource.Metadata{Name: "desktop", ResourceVersion: "7", Generation: 1},
		Spec: map[string]any{
			"machineSelector": map[string]any{"location": map[string]any{"lldp_port": "bridge/ether3", "switch_mac": "d4:01:c3:27:91:67"}},
			"packages":        []any{"curl"},
		},
	}}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	request := httptest.NewRequest(http.MethodPatch, "/api/v1alpha1/servers/desktop", strings.NewReader("packages:\n  - jq\n"))
	request.Header.Set("Content-Type", "application/merge-patch+yaml")
	request.Header.Set("If-Match", `"7"`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var patched apigen.Server
	if err := json.NewDecoder(response.Body).Decode(&patched); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if patched.Spec.Packages == nil || len(*patched.Spec.Packages) != 1 || (*patched.Spec.Packages)[0] != "jq" {
		t.Fatalf("packages = %#v, want YAML patch replacement", patched.Spec.Packages)
	}
}

func TestGetMachineIncludesValidatedStatus(t *testing.T) {
	t.Parallel()

	statusJSON, err := json.Marshal(testMachineReportSpec())
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal(statusJSON, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	storage := &fakeStore{created: resource.Resource{
		APIVersion: registry.MachineResource.APIVersion,
		Kind:       registry.MachineResource.Kind,
		Metadata:   resource.Metadata{Name: "lab-node", ResourceVersion: "7"},
		Spec: map[string]any{"location": map[string]any{
			"lldp_port":  "Ethernet1",
			"switch_mac": "00:11:22:33:44:55",
		}},
		Status: map[string]any{
			"inventory":          status,
			"phase":              "Ready",
			"observedGeneration": float64(7),
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "Reconciled",
			}},
		},
	}}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	request := httptest.NewRequest(http.MethodGet, "/api/v1alpha1/machines/lab-node", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var machine apigen.Machine
	if err := json.NewDecoder(response.Body).Decode(&machine); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if machine.Status == nil || machine.Status.Inventory == nil || machine.Status.Inventory.Cpu.ModelName != "Test CPU" {
		t.Fatalf("Machine status = %#v, want typed report observation", machine.Status)
	}
}

func TestPutMachineReportCreatesAndReplaces(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body, err := json.Marshal(apigen.MachineReportCreate{
		ApiVersion: apigen.MachineReportCreateApiVersionHomelabIov1alpha1,
		Kind:       apigen.MachineReportCreateKindMachineReport,
		Metadata:   apigen.Metadata{Name: "lab-node"},
		Spec:       testMachineReportSpec(),
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/machine-reports/lab-node", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}

	replacement := testMachineReportSpec()
	replacement.ObservedAt = time.Unix(2, 0).UTC()
	body, err = json.Marshal(apigen.MachineReportCreate{
		ApiVersion: apigen.MachineReportCreateApiVersionHomelabIov1alpha1,
		Kind:       apigen.MachineReportCreateKindMachineReport,
		Metadata:   apigen.Metadata{Name: "lab-node"},
		Spec:       replacement,
	})
	if err != nil {
		t.Fatalf("encode replacement request: %v", err)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1alpha1/machine-reports/lab-node", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("replace status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("ETag"); got != `"8"` {
		t.Fatalf("ETag = %q", got)
	}
}

func TestDeleteMachineReportCollection(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body, err := json.Marshal(apigen.MachineReportCreate{
		ApiVersion: apigen.MachineReportCreateApiVersionHomelabIov1alpha1,
		Kind:       apigen.MachineReportCreateKindMachineReport,
		Metadata:   apigen.Metadata{Name: "lab-node"},
		Spec:       testMachineReportSpec(),
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	putRequest := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/machine-reports/lab-node", bytes.NewReader(body))
	putRequest.Header.Set("Content-Type", "application/json")
	putResponse := httptest.NewRecorder()
	handler.ServeHTTP(putResponse, putRequest)
	if putResponse.Code != http.StatusCreated {
		t.Fatalf("put status = %d, body = %s", putResponse.Code, putResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodDelete, "/api/v1alpha1/machine-reports", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete collection status = %d, body = %s", response.Code, response.Body.String())
	}
	var result apigen.DeleteCollectionResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", result.Deleted)
	}
}

func testMachineReportSpec() apigen.MachineReportSpec {
	return apigen.MachineReportSpec{
		ObservedAt: time.Unix(1, 0).UTC(),
		Storage:    []apigen.MachineReportStorageDevice{},
		System:     apigen.MachineReportSystem{},
		Cpu: apigen.MachineReportCPU{
			VendorId:      "GenuineIntel",
			ModelName:     "Test CPU",
			Sockets:       1,
			PhysicalCores: 1,
			LogicalCpus:   1,
			Cores: []apigen.MachineReportCPUCore{{
				Flags:           []string{},
				Bugs:            []string{},
				PowerManagement: []string{},
			}},
		},
		Interfaces: []apigen.MachineReportNetworkInterface{},
		LLDPInfo: []apigen.MachineReportLLDPInterfaceGroup{{
			Interface: []apigen.MachineReportLLDPInterface{{
				Name: "eno1",
				Via:  "LLDP",
				RID:  "1",
				Age:  "0 day, 00:00:42",
				Chassis: []apigen.MachineReportLLDPChassis{{
					IDs:                  []apigen.MachineReportLLDPIdentifier{{Type: "mac", Value: "00:11:22:33:44:55"}},
					Names:                []apigen.MachineReportLLDPValue{{Value: "switch-1"}},
					Descriptions:         []apigen.MachineReportLLDPValue{{Value: "Lab switch"}},
					ManagementIPs:        []apigen.MachineReportLLDPValue{{Value: "192.0.2.1"}},
					ManagementInterfaces: []apigen.MachineReportLLDPValue{{Value: "1"}},
					Capabilities: []apigen.MachineReportLLDPCapability{{
						Type:    "Bridge",
						Enabled: true,
					}},
				}},
				Ports: []apigen.MachineReportLLDPPort{{
					IDs:  []apigen.MachineReportLLDPIdentifier{{Type: "ifname", Value: "Ethernet1"}},
					TTLs: []apigen.MachineReportLLDPValue{{Value: "120"}},
				}},
			}},
		}},
	}
}

func TestOpenAPIAndSwaggerUI(t *testing.T) {
	t.Parallel()

	specification, err := apigen.GetSpec()
	if err != nil {
		t.Fatalf("load embedded OpenAPI document: %v", err)
	}
	if err := specification.Validate(context.Background()); err != nil {
		t.Fatalf("validate embedded OpenAPI document: %v", err)
	}

	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeStore{}, time.Second)

	openAPIRequest := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	openAPIResponse := httptest.NewRecorder()
	handler.ServeHTTP(openAPIResponse, openAPIRequest)
	if openAPIResponse.Code != http.StatusOK {
		t.Fatalf("OpenAPI status = %d", openAPIResponse.Code)
	}
	var document map[string]any
	if err := json.NewDecoder(openAPIResponse.Body).Decode(&document); err != nil {
		t.Fatalf("decode OpenAPI document: %v", err)
	}
	if document["openapi"] != "3.1.0" {
		t.Fatalf("OpenAPI version = %v", document["openapi"])
	}

	docsRequest := httptest.NewRequest(http.MethodGet, "/docs/", nil)
	docsResponse := httptest.NewRecorder()
	handler.ServeHTTP(docsResponse, docsRequest)
	if docsResponse.Code != http.StatusOK {
		t.Fatalf("Swagger UI status = %d", docsResponse.Code)
	}
	if !strings.Contains(docsResponse.Body.String(), "/docs/swagger-ui.css") {
		t.Fatal("Swagger UI page does not reference embedded assets")
	}
	if !strings.Contains(docsResponse.Body.String(), "defaultModelsExpandDepth: 1") {
		t.Fatal("Swagger UI page does not expose component schemas")
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/docs/swagger-ui.css", nil)
	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("Swagger UI asset status = %d", assetResponse.Code)
	}
}
