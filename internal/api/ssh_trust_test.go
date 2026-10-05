package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func TestTrustReportPermissionsIdentityAndConvergence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := resource.Resource{APIVersion: resource.APIVersion, Kind: "Server", Metadata: resource.Metadata{Name: "host", UID: "server-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{"sshCertificateAuthorityRef": map[string]any{"name": "ca"}}, Status: map[string]any{"desiredSSHTrustBundleDigest": digest, "sshTrust": map[string]any{"authorityRef": map[string]any{"name": "ca", "uid": "ca-uid"}, "publicBundle": "public"}}}
	server.Spec["machineSelector"] = map[string]any{"location": map[string]any{"switch_mac": "00:11:22:33:44:55", "lldp_port": "1"}}
	authority := resource.Resource{APIVersion: resource.APIVersion, Kind: "SSHCertificateAuthority", Metadata: resource.Metadata{Name: "ca", UID: "ca-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1, "trustBundleDigest": digest}}
	s := testutil.NewStore(server, authority)
	policy := &AccessPolicy{Identities: []AccessIdentity{
		{Name: "agent", Token: "agent", Permissions: []AccessPermission{{Kind: "Server", Methods: []string{"GET"}}}},
		{Name: "runner", Token: "runner", Permissions: []AccessPermission{{Kind: "Server", Subresource: "status", Methods: []string{"PATCH"}, ResourceNames: []string{"host"}}}},
	}}
	h := WithAccessPolicy(New(slog.New(slog.NewTextHandler(io.Discard, nil)), s, time.Second), policy)
	request := func(token, path, uid, sha string) int {
		body := `{"metadata":{"uid":"` + uid + `"},"status":{"installedSSHTrustBundleDigest":"` + sha + `"}}`
		r := httptest.NewRequest("PATCH", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/merge-patch+json")
		r.Header.Set("If-Match", `"`+s.Resources["Server/host"].Metadata.ResourceVersion+`"`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	path := "/api/v1alpha1/servers/host/status"
	if got := request("agent", path, "server-uid", digest); got != 403 {
		t.Fatalf("agent status=%d", got)
	}
	if got := request("runner", "/api/v1alpha1/servers/host", "server-uid", digest); got != 403 {
		t.Fatalf("runner can edit Server: %d", got)
	}
	if got := request("runner", "/api/v1alpha1/servers/other/status", "server-uid", digest); got != 403 {
		t.Fatalf("named permission leaked: %d", got)
	}
	if got := request("runner", path, "replacement", digest); got != 409 {
		t.Fatalf("UID replacement status=%d", got)
	}
	if got := request("runner", path, "server-uid", "sha256:"+strings.Repeat("b", 64)); got != 409 {
		t.Fatalf("stale trust status=%d", got)
	}
	if got := request("runner", path, "server-uid", digest); got != http.StatusOK {
		t.Fatalf("verified trust status=%d", got)
	}
	if s.Resources["Server/host"].Status["installedSSHTrustBundleDigest"] != digest {
		t.Fatal("convergence not recorded")
	}
	writes := s.Writes
	if got := request("runner", path, "server-uid", digest); got != 200 || s.Writes != writes {
		t.Fatal("unchanged observation rewrote status")
	}
	authority.Metadata.Generation = 2
	s.Resources["SSHCertificateAuthority/ca"] = authority
	if got := request("runner", path, "server-uid", digest); got != 409 {
		t.Fatalf("unobserved authority generation accepted: %d", got)
	}
}
