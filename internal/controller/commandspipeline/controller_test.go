package commandspipeline

import (
	"strings"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
	"go.yaml.in/yaml/v3"
)

func TestRenderUsesCommandsRepositoryAndNormalRuntime(t *testing.T) {
	reconciler := &Reconciler{config: Config{
		CommandRunnerImage:     "registry.example/arch-provisioner:v1",
		PublicAPIURL:           "https://stigmergy.example",
		AnsibleRolesRepository: "https://github.com/example/roles.git",
		AnsibleRolesRevision:   "main",
	}}
	value := registry.NewCommandsPipeline(resource.Metadata{Name: "servers"}, apigen.CommandsPipelineSpec{
		CommandsRepositoryRef:    apigen.CommandsPipelineRepositoryReference{Name: "commands-data"},
		InventoryCaptureGroupRef: apigen.CommandsPipelineInventoryCaptureGroupReference{Name: "servers"},
		PipelineProviderRef:      apigen.CommandsPipelineProviderReference{Name: "concourse"},
	})
	repository := registry.NewGitRepository(resource.Metadata{Name: "commands-data"}, apigen.GitRepositorySpec{
		Url: "git@github.com:example/commands.git", Branch: "main",
	})
	keyPair := registry.NewSSHKeyPair(resource.Metadata{Name: "git-ssh-key"}, apigen.SSHKeyPairSpec{Path: "automation/git-ssh-key"})

	rendered, err := reconciler.render(value, repository, keyPair, "servers.sh")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &document); err != nil {
		t.Fatalf("rendered invalid YAML: %v", err)
	}
	for _, expected := range []string{
		"git@github.com:example/commands.git", "automation/git-ssh-key.privateKey",
		"registry.example/arch-provisioner", "tag: v1", "CONTAINER_MODE: normal",
		"INVENTORY_CAPTURE_GROUP: servers", "COMMAND_FILE: servers.sh",
		"exec /bin/bash", "user: keiichi",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered pipeline does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestSafeCommandPath(t *testing.T) {
	value := registry.NewCommandsPipeline(resource.Metadata{Name: "servers"}, apigen.CommandsPipelineSpec{
		InventoryCaptureGroupRef: apigen.CommandsPipelineInventoryCaptureGroupReference{Name: "servers"},
	})
	if got, err := safeCommandPath(value); err != nil || got != "servers.sh" {
		t.Fatalf("default path = %q, %v", got, err)
	}
	for _, invalid := range []string{"../secret", "/tmp/run.sh", "commands/../run.sh", "."} {
		value.Spec.CommandPath = &invalid
		if _, err := safeCommandPath(value); err == nil {
			t.Errorf("safeCommandPath(%q) accepted an unsafe path", invalid)
		}
	}
}
