package utils

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

func TestOpenBaoKeyStoreWritesAndDeletesSecret(t *testing.T) {
	type recordedRequest struct {
		method string
		path   string
		body   map[string]map[string]string
	}
	requests := make(chan recordedRequest, 2)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-Vault-Token") != "test-token" {
			t.Errorf("OpenBao token header is missing")
		}
		recorded := recordedRequest{method: request.Method, path: request.URL.Path}
		if request.Body != nil {
			if err := json.NewDecoder(request.Body).Decode(&recorded.body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
		}
		requests <- recorded
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	t.Setenv("OPENBAO_TOKEN", "test-token")
	prefix := "secrets"
	provider := apigen.OpenBaoSecretStoreProvider{
		Address: "http://openbao.test", KvV2Mount: "kv2", KeyPrefix: &prefix,
		Authentication: apigen.OpenBaoSecretStoreAuthentication{TokenEnvironmentVariable: stringPointer("OPENBAO_TOKEN")},
	}
	client := &OpenBaoKeyStore{provider: provider, client: &http.Client{Transport: transport}}
	if err := client.WriteSecret(t.Context(), "desktop/ssh-keys/matt", map[string]string{"secret": "value"}); err != nil {
		t.Fatalf("WriteSecret() error = %v", err)
	}
	write := <-requests
	if write.method != http.MethodPost || write.path != "/v1/kv2/data/secrets/desktop/ssh-keys/matt" {
		t.Fatalf("write request = %s %s", write.method, write.path)
	}
	if write.body["data"]["secret"] != "value" {
		t.Fatalf("write body = %#v", write.body)
	}
	if err := client.DeleteSecret(t.Context(), "desktop/ssh-keys/matt"); err != nil {
		t.Fatalf("DeleteSecret() error = %v", err)
	}
	deleted := <-requests
	if deleted.method != http.MethodDelete || deleted.path != "/v1/kv2/data/secrets/desktop/ssh-keys/matt" {
		t.Fatalf("delete request = %s %s", deleted.method, deleted.path)
	}
}

func TestOpenBaoEndpointRejectsUnsafeLogicalPath(t *testing.T) {
	provider := apigen.OpenBaoSecretStoreProvider{Address: "http://openbao.test", KvV2Mount: "kv2"}
	client := NewOpenBaoKeyStore(&provider)
	for _, logicalPath := range []string{"/secret", "secret/", "secret/../other", "secret//other"} {
		if _, err := client.GetOpenBaoKVEndpoint(logicalPath); err == nil {
			t.Fatalf("GetOpenBaoKVEndpoint(%q) error = nil", logicalPath)
		}
	}
}

func stringPointer(value string) *string { return &value }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
