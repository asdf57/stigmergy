package commandspipeline

import (
	"context"
	"github.com/asdf57/stigmergy/internal/testutil"
	"strings"
	"testing"
	"time"

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
		Url:            "git@github.com:example/commands.git",
		Authentication: &apigen.GitRepositoryAuthentication{SshKeyPairRef: "git-ssh-key"},
	})
	keyPair := registry.NewSSHKeyPair(resource.Metadata{Name: "git-ssh-key"}, apigen.SSHKeyPairSpec{Path: "automation/git-ssh-key"})

	rendered, err := reconciler.render(value, repository, keyPair, "execution-branch", "run/run.sh", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &document); err != nil {
		t.Fatalf("rendered invalid YAML: %v", err)
	}
	for _, expected := range []string{
		"git@github.com:example/commands.git", "automation/git-ssh-key.privateKey",
		"branch: execution-branch", "ref: abc123",
		"registry.example/arch-provisioner", "tag: v1", "CONTAINER_MODE: normal",
		"INVENTORY_CAPTURE_GROUP: servers", "COMMAND_FILE: run/run.sh",
		"exec /bin/bash", "user: keiichi",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("rendered pipeline does not contain %q:\n%s", expected, rendered)
		}
	}
}

func TestScheduleAndParametersStayGenericAndUnprivileged(t *testing.T) {
	r := &Reconciler{config: Config{CommandRunnerImage: "runner:latest", PublicAPIURL: "https://api.example", AnsibleRolesRepository: "https://example/roles", AnsibleRolesRevision: "main", RunnerParameters: map[string]string{"STIGMERGY_API_TOKEN": "((runner-token))"}}}
	schedule := "5m"
	v := registry.NewCommandsPipeline(resource.Metadata{Name: "test"}, apigen.CommandsPipelineSpec{InventoryCaptureGroupRef: apigen.CommandsPipelineInventoryCaptureGroupReference{Name: "test"}, Schedule: &schedule})
	repo := registry.NewGitRepository(resource.Metadata{Name: "repo"}, apigen.GitRepositorySpec{Url: "https://example/commands"})
	data, err := r.render(v, repo, registry.SSHKeyPair{}, "execution-branch", "test.sh", "abc123")
	if err != nil || strings.Contains(data, "interval: 5m") || strings.Contains(data, "trigger: true") || !strings.Contains(data, "((runner-token))") || strings.Contains(data, "privileged: true") {
		t.Fatalf("render: %s %v", data, err)
	}
	r.config.RunnerParameters["COMMAND_FILE"] = "override"
	if _, err := r.render(v, repo, registry.SSHKeyPair{}, "execution-branch", "test.sh", "abc123"); err == nil {
		t.Fatal("reserved controller parameter was overridden")
	}
}

func TestRenderPublicRepositoryOmitsPrivateKey(t *testing.T) {
	reconciler := &Reconciler{config: Config{
		CommandRunnerImage: "registry.example/arch-provisioner:latest",
		PublicAPIURL:       "https://stigmergy.example", AnsibleRolesRepository: "https://github.com/example/roles.git", AnsibleRolesRevision: "main",
	}}
	value := registry.NewCommandsPipeline(resource.Metadata{Name: "servers"}, apigen.CommandsPipelineSpec{
		InventoryCaptureGroupRef: apigen.CommandsPipelineInventoryCaptureGroupReference{Name: "servers"},
	})
	repository := registry.NewGitRepository(resource.Metadata{Name: "commands-data"}, apigen.GitRepositorySpec{
		Url: "https://github.com/example/commands.git",
	})
	rendered, err := reconciler.render(value, repository, registry.SSHKeyPair{}, "execution-branch", "servers.sh", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "private_key") {
		t.Fatalf("public repository unexpectedly contains private_key:\n%s", rendered)
	}
}

func TestScheduleCreatesFreshRequestsWithoutReplayOrOverlap(t *testing.T) {
	ctx := context.Background()
	interval := "5m"
	ttl := int64(60)
	executor := registry.NewCommandsPipeline(resource.Metadata{Name: "trust", UID: "executor-uid", ResourceVersion: "1", Generation: 1}, apigen.CommandsPipelineSpec{Schedule: &interval, CommandTemplate: &apigen.ScheduledCommandTemplate{Script: "echo trust", TtlSecondsAfterFinished: &ttl}})
	raw, _ := executor.Encode()
	s := testutil.NewStore(raw)
	r := NewReconciler(s, Config{})
	now := time.Unix(3000, 0)
	r.now = func() time.Time { return now }
	if err := r.schedule(ctx, &executor); err != nil {
		t.Fatal(err)
	}
	commands, _ := s.List(ctx, registry.CommandResource.Kind)
	if len(commands.Items) != 1 {
		t.Fatalf("commands: %v", commands)
	}
	first := commands.Items[0]
	if first.Spec["commandsPipelineRef"].(map[string]any)["name"] != "trust" {
		t.Fatal(first.Spec)
	}
	if err := r.schedule(ctx, &executor); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if err := r.schedule(ctx, &executor); err != nil {
		t.Fatal(err)
	}
	commands, _ = s.List(ctx, registry.CommandResource.Kind)
	if len(commands.Items) != 1 {
		t.Fatal("overlapping schedule was created")
	}
	// Simulate terminal TTL cleanup. Same slot must not recreate it.
	delete(s.Resources, first.Kind+"/"+first.Metadata.Name)
	if err := r.schedule(ctx, &executor); err != nil {
		t.Fatal(err)
	}
	commands, _ = s.List(ctx, registry.CommandResource.Kind)
	if len(commands.Items) != 0 {
		t.Fatal("expired Command was recreated")
	}
	now = now.Add(20 * time.Minute)
	if err := r.schedule(ctx, &executor); err != nil {
		t.Fatal(err)
	}
	commands, _ = s.List(ctx, registry.CommandResource.Kind)
	if len(commands.Items) != 1 || commands.Items[0].Metadata.Name == first.Metadata.Name {
		t.Fatal("expected one fresh request, no backfill")
	}
	// A restarted scheduler uses persisted status.
	fresh, _ := s.Get(ctx, executor.Kind, executor.Metadata.Name)
	restarted, _ := registry.CommandsPipelineResource.Decode(fresh)
	if err := NewReconciler(s, Config{}).schedule(ctx, &restarted); err != nil {
		t.Fatal(err)
	}
}
