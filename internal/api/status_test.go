package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestGenericStatusPatchAuthorizationSchemaAndConcurrency(t *testing.T) {
	for _, tc := range []struct {
		name, kind, plural, token, etag, body string
		want                                  int
	}{
		{"server-broad-status", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{"phase":"Updated"}}`, 200},
		{"secret-generic-status", "Secret", "secrets", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{"phase":"Ready"}}`, 200},
		{"no-spec-permission", "Server", "servers", "reader", `"1"`, `{"metadata":{"uid":"uid"},"status":{"phase":"Updated"}}`, 403},
		{"forbid-spec", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{},"spec":{}}`, 422},
		{"forbid-metadata-edit", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"uid","labels":{"x":"y"}},"status":{}}`, 422},
		{"unknown-status-field", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{"madeUp":true}}`, 422},
		{"invalid-status-type", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{"observedGeneration":"bad"}}`, 422},
		{"invalid-status-enum", "Secret", "secrets", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{"phase":"Invalid"}}`, 422},
		{"missing-etag", "Server", "servers", "writer", "", `{"metadata":{"uid":"uid"},"status":{}}`, 428},
		{"stale-etag", "Server", "servers", "writer", `"2"`, `{"metadata":{"uid":"uid"},"status":{}}`, 409},
		{"replaced-lifetime", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"old"},"status":{}}`, 409},
		{"missing-uid", "Server", "servers", "writer", `"1"`, `{"status":{}}`, 422},
		{"no-op", "Server", "servers", "writer", `"1"`, `{"metadata":{"uid":"uid"},"status":{"phase":"Pending"}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{}
			if tc.kind == "Server" {
				spec["machineSelector"] = map[string]any{"location": map[string]any{"switch_mac": "00:11:22:33:44:55", "lldp_port": "1"}}
			}
			if tc.kind == "Secret" {
				spec = map[string]any{"secretStoreRef": map[string]any{"name": "bao"}, "path": "test", "data": map[string]any{"value": "public"}}
			}
			original := resource.Resource{APIVersion: resource.APIVersion, Kind: tc.kind, Metadata: resource.Metadata{Name: "host", UID: "uid", ResourceVersion: "1", Generation: 1}, Spec: spec, Status: map[string]any{"phase": "Pending", "observedGeneration": 1}}
			s := testutil.NewStore(original)
			policy := &AccessPolicy{Identities: []AccessIdentity{
				{Name: "writer", Token: "writer", Permissions: []AccessPermission{{Kind: tc.kind, Subresource: "status", Methods: []string{"PATCH"}}}},
				{Name: "reader", Token: "reader", Permissions: []AccessPermission{{Kind: tc.kind, Methods: []string{"GET", "PATCH"}}}},
			}}
			h := WithAccessPolicy(New(slog.New(slog.NewTextHandler(io.Discard, nil)), s, time.Second), policy)
			r := httptest.NewRequest("PATCH", "/api/v1alpha1/"+tc.plural+"/host/status", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/merge-patch+json")
			r.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.etag != "" {
				r.Header.Set("If-Match", tc.etag)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			updated := s.Resources[tc.kind+"/host"]
			if !resource.EqualJSON(updated.Spec, original.Spec) || updated.Metadata.Generation != original.Metadata.Generation {
				t.Fatal("status update modified spec/generation")
			}
			if !resource.EqualJSON(updated.Status["observedGeneration"], 1) {
				t.Fatal("status merge overwrote another writer's field")
			}
			if (tc.want != 200 || tc.name == "no-op") && s.Writes != 0 {
				t.Fatal("invalid/no-op patch wrote status")
			}
			if tc.want == 200 && w.Header().Get("ETag") == "" {
				t.Fatal("missing result ETag")
			}
		})
	}
}
