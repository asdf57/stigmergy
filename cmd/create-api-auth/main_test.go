package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asdf57/stigmergy/internal/api"
)

func TestGeneratePrivateDistinctCredentialsWithoutOverwriting(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "credentials")
	if err := generate(directory); err != nil {
		t.Fatal(err)
	}
	policy, err := api.LoadAccessPolicy(filepath.Join(directory, "api-access.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Identities) != 3 {
		t.Fatal("expected independent admin/agent/runner identities")
	}
	for _, filename := range []string{"api-access.json", "bootstrap.env", "admin-token", "agent-token", "runner-token"} {
		info, err := os.Stat(filepath.Join(directory, filename))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s must be private: %v", filename, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(directory, "api-access.json"))
	if err := generate(directory); err == nil {
		t.Fatal("must not replace credentials on rerun")
	}
	after, _ := os.ReadFile(filepath.Join(directory, "api-access.json"))
	if string(before) != string(after) {
		t.Fatal("existing policy changed")
	}
}

func TestOperatorPolicyPreservesTokensAndAddsOnlyPublicDependencyReads(t *testing.T) {
	source := filepath.Join(t.TempDir(), "original")
	if err := generate(source); err != nil {
		t.Fatal(err)
	}
	prior, _ := api.LoadAccessPolicy(filepath.Join(source, "api-access.json"))
	// Start from a policy without the new host-key read grant.
	prior.Identities[2].Permissions = []api.AccessPermission{{Kind: "InventoryCaptureGroup", Methods: []string{"GET"}}, {Kind: "Server", Methods: []string{"GET"}}, {Kind: "Server", Subresource: "status", Methods: []string{"PATCH"}}}
	fixture, _ := json.Marshal(prior)
	if err := os.WriteFile(filepath.Join(source, "api-access.json"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "operators")
	if err := prepareOperatorPolicy(filepath.Join(source, "api-access.json"), target); err != nil {
		t.Fatal(err)
	}
	original, _ := api.LoadAccessPolicy(filepath.Join(source, "api-access.json"))
	updated, _ := api.LoadAccessPolicy(filepath.Join(target, "api-access.json"))
	for i, identity := range original.Identities {
		if updated.Identities[i].Token != identity.Token {
			t.Fatal("rotated an existing token")
		}
		expected := len(identity.Permissions)
		if identity.Name == "runner" {
			expected += 5
		}
		if len(updated.Identities[i].Permissions) != expected {
			t.Fatal("unexpected grant")
		}
	}
	grant := updated.Identities[2].Permissions[len(updated.Identities[2].Permissions)-1]
	if grant.Kind != "Command" || len(grant.Methods) != 1 || grant.Methods[0] != "GET" {
		t.Fatal("wrong public-key grant")
	}
	if err := prepareOperatorPolicy(filepath.Join(source, "api-access.json"), target); err == nil {
		t.Fatal("overwrote existing policy")
	}
}

func TestPrepareBootstrapPreservesSettingsAndNeverOverwrites(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "credentials")
	if err := generate(directory); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source.env")
	if err := os.WriteFile(source, []byte("CONCOURSE_PASSWORD=fixture\nSTIGMERGY_API_TOKEN=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "private", "init.env")
	if err := prepareBootstrap(directory, source, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "CONCOURSE_PASSWORD=fixture\n") || strings.Contains(string(data), "TOKEN=old") || !strings.Contains(string(data), "STIGMERGY_RUNNER_API_TOKEN=") || !strings.Contains(string(data), "STIGMERGY_AGENT_API_TOKEN=") {
		t.Fatal("incorrect credential composition")
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0600 {
		t.Fatal("env-file not private")
	}
	if err := prepareBootstrap(directory, source, output); err == nil {
		t.Fatal("existing env-file overwritten")
	}
}
