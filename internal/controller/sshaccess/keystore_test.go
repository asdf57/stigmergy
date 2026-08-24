package sshaccess

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

func TestOpenBaoKeyStoreCreatesOnceAndAdoptsOwnedKey(t *testing.T) {
	var stored map[string]any
	writes := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		expectedPath := "/v1/kv2/data/secrets/desktop/ssh-keys/matt"
		if request.Method == http.MethodDelete {
			expectedPath = "/v1/kv2/metadata/secrets/desktop/ssh-keys/matt"
		}
		if request.URL.Path != expectedPath {
			t.Errorf("request path = %q", request.URL.Path)
		}
		if request.Header.Get("X-Vault-Token") != "test-token" {
			t.Errorf("OpenBao token header is missing")
		}
		switch request.Method {
		case http.MethodGet:
			if stored == nil {
				return jsonResponse(http.StatusNotFound, map[string]any{"errors": []string{}}), nil
			}
			return jsonResponse(http.StatusOK, map[string]any{"data": map[string]any{
				"data": stored, "metadata": map[string]any{"version": 1},
			}}), nil
		case http.MethodPost:
			writes++
			var body struct {
				Options map[string]any `json:"options"`
				Data    map[string]any `json:"data"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if body.Options["cas"] != float64(0) {
				t.Errorf("CAS option = %#v", body.Options["cas"])
			}
			stored = body.Data
			return jsonResponse(http.StatusOK, map[string]any{}), nil
		case http.MethodDelete:
			stored = nil
			return jsonResponse(http.StatusNoContent, map[string]any{}), nil
		default:
			return jsonResponse(http.StatusMethodNotAllowed, map[string]any{}), nil
		}
	})

	keyStore := &OpenBaoKeyStore{
		client: &http.Client{Transport: transport},
		lookupEnv: func(name string) (string, bool) {
			return "test-token", name == "OPENBAO_TOKEN"
		},
	}
	provider := apigen.OpenBaoSecretStoreProvider{
		Address: "http://openbao.test:8200", KvV2Mount: "kv2",
		Authentication: apigen.OpenBaoSecretStoreAuthentication{TokenEnvironmentVariable: stringPointer("OPENBAO_TOKEN")},
	}
	owner := KeyOwnership{ServerUID: "server-uid", GrantUID: "grant-uid"}

	first, err := keyStore.EnsureKeyPair(context.Background(), provider, "secrets/desktop/ssh-keys/matt", owner)
	if err != nil {
		t.Fatalf("EnsureKeyPair() error = %v", err)
	}
	if writes != 1 || first.Version != 1 || !strings.HasPrefix(first.PublicKey, "ssh-ed25519 ") || first.Fingerprint == "" {
		t.Fatalf("first key = %#v, writes = %d", first, writes)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(stored["publicKey"].(string))); err != nil {
		t.Fatalf("stored public key is invalid: %v", err)
	}
	if !strings.Contains(stored["privateKey"].(string), "OPENSSH PRIVATE KEY") {
		t.Fatalf("stored private key is not OpenSSH PEM")
	}

	second, err := keyStore.EnsureKeyPair(context.Background(), provider, "secrets/desktop/ssh-keys/matt", owner)
	if err != nil {
		t.Fatalf("second EnsureKeyPair() error = %v", err)
	}
	if writes != 1 || second != first {
		t.Fatalf("second key = %#v, writes = %d", second, writes)
	}

	_, err = keyStore.EnsureKeyPair(context.Background(), provider, "secrets/desktop/ssh-keys/matt", KeyOwnership{ServerUID: "different", GrantUID: "grant-uid"})
	if err == nil || !strings.Contains(err.Error(), "ownership conflict") {
		t.Fatalf("ownership error = %v", err)
	}
	if err := keyStore.DeleteKeyPair(context.Background(), provider, "secrets/desktop/ssh-keys/matt", owner); err != nil {
		t.Fatalf("DeleteKeyPair() error = %v", err)
	}
	if stored != nil {
		t.Fatalf("stored key pair still exists: %#v", stored)
	}
}

func TestAuthenticationTokenReadsAgentSinkFile(t *testing.T) {
	keyStore := &OpenBaoKeyStore{
		lookupEnv: func(string) (string, bool) { return "", false },
		readFile: func(path string) ([]byte, error) {
			if path != "/run/openbao/token" {
				t.Fatalf("token path = %q", path)
			}
			return []byte("agent-token\n"), nil
		},
	}
	token, err := keyStore.authenticationToken(apigen.OpenBaoSecretStoreAuthentication{TokenFile: stringPointer("/run/openbao/token")})
	if err != nil {
		t.Fatalf("authenticationToken() error = %v", err)
	}
	if token != "agent-token" {
		t.Fatalf("token = %q", token)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(status int, value any) *http.Response {
	encoded, _ := json.Marshal(value)
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(encoded))),
	}
}

func stringPointer(value string) *string { return &value }

func TestLogicalKeyPathRejectsUnsafePrefix(t *testing.T) {
	for _, prefix := range []string{"/secrets", "secrets/", "secrets/../other", "secrets//other"} {
		if _, err := logicalKeyPath(prefix, "desktop", "matt"); err == nil {
			t.Fatalf("logicalKeyPath(%q) error = nil", prefix)
		}
	}
}
