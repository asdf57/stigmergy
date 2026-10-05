package gitpublication

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestOwnedPublicationNoopConflictAndCleanup(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "inputs.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}
	p := &GitPublisher{Now: time.Now}
	ctx := context.Background()
	repo := apigen.GitRepositorySpec{Url: remote}
	seed := PublishRequest{Repository: repo, Branch: "main", RootPath: ".", PreserveUnmanaged: true, Artifacts: []Artifact{{Path: "unrelated.txt", Content: []byte("keep")}}}
	if _, err := p.Publish(ctx, seed); err != nil {
		t.Fatal(err)
	}
	request := PublishRequest{Repository: repo, Branch: "main", RootPath: "images/test", OwnerUID: "original", Artifacts: []Artifact{{Path: "image.yaml", Content: []byte("inputs")}}}
	first, err := p.Publish(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Publish(ctx, request)
	if err != nil || second.Changed || second.Revision != first.Revision {
		t.Fatalf("no-op: %+v %v", second, err)
	}
	request.OwnerUID = "replacement"
	if _, err := p.Publish(ctx, request); err == nil {
		t.Fatal("replacement adopted owned directory")
	}
	request.RemoveOwned = true
	if _, err := p.Publish(ctx, request); err == nil {
		t.Fatal("replacement deleted owned directory")
	}
	request.OwnerUID = "original"
	result, err := p.Publish(ctx, request)
	if err != nil || !result.Changed {
		t.Fatalf("cleanup: %+v %v", result, err)
	}
	repeated, err := p.Publish(ctx, request)
	if err != nil || repeated.Changed || repeated.Revision != result.Revision {
		t.Fatalf("repeated cleanup: %+v %v", repeated, err)
	}
	repository, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := repository.Reference(plumbing.NewBranchReferenceName("main"), true)
	commit, _ := repository.CommitObject(ref.Hash())
	tree, _ := commit.Tree()
	if _, err := tree.File("unrelated.txt"); err != nil {
		t.Fatal("cleanup removed unrelated content", err)
	}
	if _, err := tree.File("images/test/.stigmergy-owner"); err == nil {
		t.Fatal("owner marker was not deleted")
	}
	request.RootPath = "."
	request.RemoveOwned = false
	if _, err := p.Publish(ctx, request); err == nil {
		t.Fatal("unowned repository root was adopted")
	}
}
