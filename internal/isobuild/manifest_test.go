package isobuild

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
)

func TestManifestSelectionMatchesContentLifetimeAndStartOrder(t *testing.T) {
	manifests := map[string]Manifest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "ls" {
			files := []map[string]string{{"href": "https://evil.invalid/steal"}, {"href": "../other.json"}}
			for id := range manifests {
				files = append(files, map[string]string{"href": id + ".json"})
			}
			json.NewEncoder(w).Encode(map[string]any{"files": files})
			return
		}
		id := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".json")
		value, ok := manifests[id]
		if !ok {
			t.Errorf("unexpected URL: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	makeManifest := func(id, content string, age time.Duration) Manifest {
		value := Manifest{ISOUID: "iso-uid", InputContent: content, InputRevision: "unrelated-shared-branch-head", TrustBundleDigest: "digest", BuildID: id, BuildStartedAt: time.Now().Add(-age), SourceRevisions: map[string]string{"builder": "builder-revision", "homelabd": "daemon-revision"}}
		for _, kind := range []apigen.ISOArtifactType{"iso", "kernel", "initrd", "rootfs"} {
			value.Artifacts = append(value.Artifacts, apigen.ISOArtifact{Type: kind, Url: server.URL + "/iso-resources/iso-uid/builds/" + id + "/" + string(kind), Sha256: strings.Repeat("a", 64)})
		}
		return value
	}
	ids := []string{"00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000003", "00000000-0000-0000-0000-000000000004"}
	manifests[ids[0]] = makeManifest(ids[0], "current", time.Hour)
	manifests[ids[1]] = makeManifest(ids[1], "current", time.Minute)
	manifests[ids[2]] = makeManifest(ids[2], "stale", time.Second)
	manifests[ids[3]] = makeManifest(ids[3], "current", time.Second)
	replacement := manifests[ids[3]]
	replacement.ISOUID = "replaced-lifetime"
	manifests[ids[3]] = replacement
	reader := &CopypartyReader{BaseURL: server.URL, Client: server.Client()}
	latest, err := reader.Latest(context.Background(), "iso-uid", "current", "digest")
	if err != nil || latest == nil || latest.BuildID != ids[1] {
		t.Fatalf("selection: %+v %v", latest, err)
	}
	latest, err = reader.Latest(context.Background(), "iso-uid", "new-input", "digest")
	if err != nil || latest != nil {
		t.Fatalf("stale inputs accepted: %+v %v", latest, err)
	}
	unsafe := manifests[ids[1]]
	unsafe.Artifacts[0].Url = server.URL + "/iso-resources/iso-uid/builds/" + ids[1] + "/%2e%2e/private"
	manifests[ids[1]] = unsafe
	if _, err := reader.Latest(context.Background(), "iso-uid", "current", "digest"); err == nil {
		t.Fatal("encoded traversal accepted")
	}
}
