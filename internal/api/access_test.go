package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccessPolicyConfinesAgentAndRunner(t *testing.T) {
	policy := &AccessPolicy{Identities: []AccessIdentity{
		{Name: "agent", Token: "agent-token", Permissions: []AccessPermission{{Kind: "MachineReport", Methods: []string{"POST", "PUT"}}, {Kind: "Server", Methods: []string{"GET"}}}},
		{Name: "runner", Token: "runner-token", Permissions: []AccessPermission{{Kind: "Secret", Methods: []string{"GET"}, ResourceNames: []string{"ansible-runner"}}}},
	}}
	handler := WithAccessPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), policy)
	cases := []struct {
		method, path, token string
		status              int
	}{
		{"POST", "/api/v1alpha1/machine-reports", "agent-token", 204},
		{"GET", "/api/v1alpha1/servers", "agent-token", 204},
		{"PUT", "/api/v1alpha1/servers/desktop", "agent-token", 403},
		{"GET", "/api/v1alpha1/secrets", "agent-token", 403},
		{"GET", "/api/v1alpha1/secrets/ca-key", "agent-token", 403},
		{"PUT", "/api/v1alpha1/ssh-certificate-authorities/ca", "agent-token", 403},
		{"PUT", "/api/v1alpha1/ssh-certificates/runner", "agent-token", 403},
		{"PUT", "/api/v1alpha1/ssh-certificates/runner", "runner-token", 403},
		{"DELETE", "/api/v1alpha1/resources", "agent-token", 403},
		{"GET", "/api/v1alpha1/secrets/ansible-runner", "runner-token", 204},
		{"GET", "/api/v1alpha1/secrets/ca-key", "runner-token", 403},
		{"GET", "/api/v1alpha1/secrets", "runner-token", 403},
		{"GET", "/api/v1alpha1/servers", "", 401},
		{"GET", "/api/v1alpha1/servers", "wrong-token", 401},
		{"GET", "/healthz", "", 204},
		{"GET", "/readyz", "", 204},
		{"GET", "/openapi.json", "", 204},
		{"GET", "/docs/", "", 204},
		{"GET", "/ipxe/00:11:22:33:44:55", "", 204},
		{"POST", "/healthz", "", 401},
		{"GET", "/future-sensitive-route", "", 401},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path+tc.token, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("got %d, want %d", response.Code, tc.status)
			}
		})
	}
}
