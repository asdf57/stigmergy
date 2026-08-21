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
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	created resource.Resource
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

func (f *fakeStore) Update(_ context.Context, value resource.Resource, _ int64) (resource.Resource, error) {
	value.Metadata.ResourceVersion = "8"
	f.created = value
	return value, nil
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

func TestPutMachineReportCreatesAndReplaces(t *testing.T) {
	t.Parallel()

	storage := &fakeStore{}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body, err := json.Marshal(testMachineReportSpec())
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
	body, err := json.Marshal(testMachineReportSpec())
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

	assetRequest := httptest.NewRequest(http.MethodGet, "/docs/swagger-ui.css", nil)
	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("Swagger UI asset status = %d", assetResponse.Code)
	}
}
