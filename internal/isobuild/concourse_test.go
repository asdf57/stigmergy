package isobuild

import (
	"strings"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
	"go.yaml.in/yaml/v3"
)

func TestPipelineUsesSnapshotsSourcesAndVersionedScripts(t *testing.T) {
	image := registry.NewISO(resource.Metadata{Name: "test", UID: "test-uid"}, apigen.ISOSpec{Distribution: "debian", Version: "trixie", Architecture: "amd64", BootMode: "uefi", BuildInputs: apigen.ISOBuildInputs{Branch: "main", Path: "images/test"}})
	repo := registry.NewGitRepository(resource.Metadata{Name: "inputs"}, apigen.GitRepositorySpec{Url: "git@example:inputs.git"})
	c := Config{BuilderRepository: "https://example/builder", BuilderBranch: "main", DaemonRepository: "https://example/daemon", DaemonBranch: "main", PublicAPIURL: "https://api.example", ArtifactBaseURL: "https://files.example", UploadPasswordVariable: "upload"}
	result, err := RenderConcourse(image, repo, "automation/git", c)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"images/test/**", "roles/os/files/**", "((automation/git.privateKey))", "ci/build-image.sh", "ci/publish.py", "serial: true", "((upload))", "HOMELABD_API_TOKEN: ((stigmergy-agent-token))"} {
		if !strings.Contains(result, expected) {
			t.Fatal("missing", expected)
		}
	}
	if strings.Count(result, "trigger: true") != 3 || strings.Contains(result, "ssh-key-pairs/") || strings.Contains(result, "curl ") {
		t.Fatal("pipeline must consume snapshots, not live key APIs")
	}
	c.BuilderRepository = "git@example:builder.git"
	if _, err := RenderConcourse(image, repo, "", c); err == nil {
		t.Fatal("unauthenticated SSH builder source accepted")
	}
}
