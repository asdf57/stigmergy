package publication

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

func TestGitPublisherPublishesAndSkipsUnchangedContent(t *testing.T) {
	remotePath := filepath.Join(t.TempDir(), "inventory.git")
	if _, err := git.PlainInit(remotePath, true); err != nil {
		t.Fatalf("initialize bare remote: %v", err)
	}
	publisher := &GitPublisher{
		LookupEnv: os.LookupEnv,
		Now:       func() time.Time { return time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC) },
	}
	request := PublishRequest{
		Repository:      apigen.GitRepositorySpec{Url: remotePath, Branch: "main"},
		PublicationName: "servers-git",
		RootPath:        "inventories/servers",
		Artifacts: []Artifact{
			{Path: "inventory.yaml", Content: []byte("all:\n  hosts: {}\n")},
			{Path: "group_vars/all.yaml", Content: []byte("ansible_user: matt\n")},
		},
	}

	first, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if !first.Changed || first.Revision == "" {
		t.Fatalf("first Publish() = %#v", first)
	}

	checkoutPath := filepath.Join(t.TempDir(), "checkout")
	if _, err := git.PlainClone(checkoutPath, false, &git.CloneOptions{
		URL: remotePath, ReferenceName: plumbing.NewBranchReferenceName("main"), SingleBranch: true,
	}); err != nil {
		t.Fatalf("clone published remote: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(checkoutPath, "inventories", "servers", "inventory.yaml"))
	if err != nil {
		t.Fatalf("read published inventory: %v", err)
	}
	if string(content) != string(request.Artifacts[0].Content) {
		t.Fatalf("published content = %q, want %q", content, request.Artifacts[0].Content)
	}
	groupVars, err := os.ReadFile(filepath.Join(checkoutPath, "inventories", "servers", "group_vars", "all.yaml"))
	if err != nil || string(groupVars) != string(request.Artifacts[1].Content) {
		t.Fatalf("published group vars = %q, error = %v", groupVars, err)
	}

	second, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second.Changed || second.Revision != first.Revision {
		t.Fatalf("second Publish() = %#v, want unchanged revision %q", second, first.Revision)
	}

	request.Artifacts = request.Artifacts[:1]
	third, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("third Publish() error = %v", err)
	}
	if !third.Changed || third.Revision == second.Revision {
		t.Fatalf("third Publish() = %#v, want changed revision", third)
	}
	updatedCheckout := filepath.Join(t.TempDir(), "updated-checkout")
	if _, err := git.PlainClone(updatedCheckout, false, &git.CloneOptions{
		URL: remotePath, ReferenceName: plumbing.NewBranchReferenceName("main"), SingleBranch: true,
	}); err != nil {
		t.Fatalf("clone updated remote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(updatedCheckout, "inventories", "servers", "group_vars", "all.yaml")); !os.IsNotExist(err) {
		t.Fatalf("stale group vars still exist, error = %v", err)
	}
}

func TestSafeRepositoryPathRejectsTraversalAndGitMetadata(t *testing.T) {
	for _, value := range []string{"../inventory.yaml", "dir/../inventory.yaml", "/inventory.yaml", ".git/config"} {
		if _, err := safeRepositoryPath(value); err == nil {
			t.Fatalf("safeRepositoryPath(%q) error = nil", value)
		}
	}
}

func TestValidateArtifactsRejectsDuplicateAndUnsafePaths(t *testing.T) {
	for _, artifacts := range [][]Artifact{
		{{Path: "../inventory.yaml", Content: []byte("bad")}},
		{{Path: "inventory.yaml"}, {Path: "inventory.yaml"}},
	} {
		if _, err := validateArtifacts(artifacts); err == nil {
			t.Fatalf("validateArtifacts(%#v) error = nil", artifacts)
		}
	}
}
