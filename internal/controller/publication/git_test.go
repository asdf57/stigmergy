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
		Path:            "ansible/inventory/homelab.yaml",
		Content:         []byte("servers:\n  hosts: {}\n"),
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
	content, err := os.ReadFile(filepath.Join(checkoutPath, "ansible", "inventory", "homelab.yaml"))
	if err != nil {
		t.Fatalf("read published inventory: %v", err)
	}
	if string(content) != string(request.Content) {
		t.Fatalf("published content = %q, want %q", content, request.Content)
	}

	second, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second.Changed || second.Revision != first.Revision {
		t.Fatalf("second Publish() = %#v, want unchanged revision %q", second, first.Revision)
	}
}

func TestSafeRepositoryPathRejectsTraversalAndGitMetadata(t *testing.T) {
	for _, value := range []string{"../inventory.yaml", "dir/../inventory.yaml", "/inventory.yaml", ".git/config"} {
		if _, err := safeRepositoryPath(value); err == nil {
			t.Fatalf("safeRepositoryPath(%q) error = nil", value)
		}
	}
}
