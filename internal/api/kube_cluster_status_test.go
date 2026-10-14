package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestKubeClusterTypedObservationsUseGenericStatusRoute(t *testing.T) {
	const valid = `{"metadata":{"uid":"cluster-uid"},"status":{"observedGeneration":1,"lastVerifiedAt":"2026-10-13T00:00:00Z","configurationRef":{"name":"setup","uid":"setup-uid"},"nodes":[{"serverRef":{"name":"zima","uid":"server-uid"},"nodeName":"zima","nodeUID":"node-uid","role":"control-plane","ready":true,"internalIP":"192.0.2.1","podCIDR":"172.28.0.0/24","kubeletVersion":"v1.37.1","containerdVersion":"2.4.1"}],"checks":{"serviceDNS":true,"podConnectivity":true}}}`
	for _, testcase := range []struct {
		name, body string
		code       int
	}{
		{"verified observations", valid, 200},
		{"no private fields", strings.Replace(valid, `"observedGeneration":1`, `"kubeconfig":"private","observedGeneration":1`, 1), 422},
		{"typed readiness", strings.Replace(valid, `"ready":true`, `"ready":"true"`, 1), 422},
		{"node identity required", strings.Replace(valid, `"nodeUID":"node-uid",`, "", 1), 422},
		{"replaced cluster", strings.Replace(valid, `"uid":"cluster-uid"`, `"uid":"old-cluster"`, 1), 409},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			var spec map[string]any
			if err := json.Unmarshal([]byte(`{"kubernetesVersion":"1.37.1","containerdVersion":"2.4.1","controlPlaneEndpoint":"lab.example.test:6443","podCIDR":"172.28.0.0/16","serviceCIDR":"172.29.0.0/16","network":{"provider":"flannel","version":"0.28.10"}}`), &spec); err != nil {
				t.Fatal(err)
			}
			cluster := resource.Resource{APIVersion: resource.APIVersion, Kind: "KubeCluster", Metadata: resource.Metadata{Name: "lab", UID: "cluster-uid", ResourceVersion: "1", Generation: 1}, Spec: spec, Status: map[string]any{}}
			storage := testutil.NewStore(cluster)
			policy := &AccessPolicy{Identities: []AccessIdentity{{Name: "operator", Token: "fixture", Permissions: []AccessPermission{{Kind: "KubeCluster", Subresource: "status", Methods: []string{"PATCH"}}}}}}
			handler := WithAccessPolicy(New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second), policy)
			request := httptest.NewRequest("PATCH", "/api/v1alpha1/kube-clusters/lab/status", strings.NewReader(testcase.body))
			request.Header.Set("Authorization", "Bearer fixture")
			request.Header.Set("If-Match", `"1"`)
			request.Header.Set("Content-Type", "application/merge-patch+json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testcase.code {
				t.Fatalf("status = %d, want %d: %s", response.Code, testcase.code, response.Body.String())
			}
			updated := storage.Resources["KubeCluster/lab"]
			if updated.Metadata.Generation != 1 || !resource.EqualJSON(updated.Spec, spec) {
				t.Fatal("observation changed desired state")
			}
			if testcase.code != 200 && storage.Writes != 0 {
				t.Fatal("invalid observation wrote status")
			}
		})
	}
}
