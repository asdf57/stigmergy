package publication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

type PublishRequest struct {
	Repository      apigen.GitRepositorySpec
	PublicationName string
	RootPath        string
	Artifacts       []Artifact
}

type Artifact struct {
	Path    string
	Content []byte
}

type PublishResult struct {
	Revision string
	URL      string
	Changed  bool
}

type Publisher interface {
	Publish(context.Context, PublishRequest) (PublishResult, error)
}

type GitPublisher struct {
	LookupEnv func(string) (string, bool)
	Now       func() time.Time
}

func NewGitPublisher() *GitPublisher {
	return &GitPublisher{LookupEnv: os.LookupEnv, Now: time.Now}
}

func (p *GitPublisher) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	repositoryPath, err := safeRepositoryPath(request.RootPath)
	if err != nil {
		return PublishResult{}, err
	}
	artifacts, err := validateArtifacts(request.Artifacts)
	if err != nil {
		return PublishResult{}, err
	}
	auth, err := p.authentication(request.Repository)
	if err != nil {
		return PublishResult{}, err
	}
	temporaryDirectory, err := os.MkdirTemp("", "stigmergy-inventory-publication-")
	if err != nil {
		return PublishResult{}, fmt.Errorf("create Git worktree: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporaryDirectory) }()

	branch := plumbing.NewBranchReferenceName(request.Repository.Branch)
	repository, err := git.PlainCloneContext(ctx, temporaryDirectory, false, &git.CloneOptions{
		URL:           request.Repository.Url,
		Auth:          auth,
		ReferenceName: branch,
		SingleBranch:  true,
	})
	emptyRemote := errors.Is(err, transport.ErrEmptyRemoteRepository)
	if err != nil && !emptyRemote {
		return PublishResult{}, fmt.Errorf("clone Git repository: %w", err)
	}
	if emptyRemote {
		repository, err = git.PlainInitWithOptions(temporaryDirectory, &git.PlainInitOptions{
			Bare: false,
			InitOptions: git.InitOptions{
				DefaultBranch: branch,
			},
		})
		if err != nil {
			return PublishResult{}, fmt.Errorf("initialize worktree for empty Git repository: %w", err)
		}
		if _, err := repository.CreateRemote(&config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{request.Repository.Url}}); err != nil {
			return PublishResult{}, fmt.Errorf("configure empty Git repository remote: %w", err)
		}
	}

	worktree, err := repository.Worktree()
	if err != nil {
		return PublishResult{}, fmt.Errorf("open Git worktree: %w", err)
	}
	rootPath := filepath.Join(temporaryDirectory, filepath.FromSlash(repositoryPath))
	unchanged, err := directoryMatches(rootPath, artifacts)
	if err != nil {
		return PublishResult{}, err
	}
	if unchanged {
		head, err := repository.Head()
		if err != nil {
			return PublishResult{}, fmt.Errorf("read unchanged Git revision: %w", err)
		}
		return PublishResult{Revision: head.Hash().String(), URL: request.Repository.Url, Changed: false}, nil
	}
	if err := rejectSymlinkParents(temporaryDirectory, repositoryPath); err != nil {
		return PublishResult{}, err
	}
	if err := os.RemoveAll(rootPath); err != nil {
		return PublishResult{}, fmt.Errorf("clear owned publication directory: %w", err)
	}
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		return PublishResult{}, fmt.Errorf("create publication directory: %w", err)
	}
	for _, artifact := range artifacts {
		filePath := filepath.Join(rootPath, filepath.FromSlash(artifact.Path))
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return PublishResult{}, fmt.Errorf("create artifact directory for %q: %w", artifact.Path, err)
		}
		if err := os.WriteFile(filePath, artifact.Content, 0o644); err != nil {
			return PublishResult{}, fmt.Errorf("write artifact %q: %w", artifact.Path, err)
		}
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true, Path: repositoryPath}); err != nil {
		return PublishResult{}, fmt.Errorf("stage publication directory: %w", err)
	}

	authorName, authorEmail, message := commitMetadata(request.Repository, request.PublicationName)
	commit, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{
		Name: authorName, Email: authorEmail, When: p.Now().UTC(),
	}})
	if err != nil {
		return PublishResult{}, fmt.Errorf("commit publication artifacts: %w", err)
	}
	refspec := config.RefSpec(fmt.Sprintf("%s:%s", branch, branch))
	if err := repository.PushContext(ctx, &git.PushOptions{Auth: auth, RefSpecs: []config.RefSpec{refspec}}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return PublishResult{}, fmt.Errorf("push inventory commit: %w", err)
	}
	return PublishResult{Revision: commit.String(), URL: request.Repository.Url, Changed: true}, nil
}

func validateArtifacts(artifacts []Artifact) ([]Artifact, error) {
	if len(artifacts) == 0 {
		return nil, errors.New("publication must contain at least one artifact")
	}
	validated := make([]Artifact, len(artifacts))
	seen := make(map[string]struct{}, len(artifacts))
	for index, artifact := range artifacts {
		cleaned, err := safeRepositoryPath(artifact.Path)
		if err != nil {
			return nil, fmt.Errorf("invalid artifact path: %w", err)
		}
		if _, exists := seen[cleaned]; exists {
			return nil, fmt.Errorf("artifact path %q is duplicated", cleaned)
		}
		seen[cleaned] = struct{}{}
		validated[index] = Artifact{Path: cleaned, Content: artifact.Content}
	}
	return validated, nil
}

func directoryMatches(root string, artifacts []Artifact) (bool, error) {
	desired := make(map[string][]byte, len(artifacts))
	for _, artifact := range artifacts {
		desired[artifact.Path] = artifact.Content
	}
	found := 0
	matches := true
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("owned publication directory contains a symbolic link")
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		expected, exists := desired[relative]
		if !exists {
			matches = false
			found++
			return nil
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		if !bytes.Equal(content, expected) {
			matches = false
			return nil
		}
		found++
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect publication directory: %w", err)
	}
	return matches && found == len(artifacts), nil
}

func rejectSymlinkParents(repositoryRoot, repositoryPath string) error {
	current := repositoryRoot
	parts := strings.Split(repositoryPath, "/")
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect publication path parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("publication path parent %q is a symbolic link", part)
		}
	}
	return nil
}

func (p *GitPublisher) authentication(repository apigen.GitRepositorySpec) (transport.AuthMethod, error) {
	if repository.Authentication == nil {
		return nil, nil
	}
	password, found := p.LookupEnv(repository.Authentication.PasswordEnvironmentVariable)
	if !found || password == "" {
		return nil, fmt.Errorf("Git credential environment variable %q is not set", repository.Authentication.PasswordEnvironmentVariable)
	}
	username := "x-access-token"
	if repository.Authentication.Username != nil {
		username = *repository.Authentication.Username
	}
	return &githttp.BasicAuth{Username: username, Password: password}, nil
}

func safeRepositoryPath(value string) (string, error) {
	cleaned := path.Clean(value)
	if value == "" || cleaned != value || path.IsAbs(value) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("publication path %q must be a canonical relative path", value)
	}
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return "", fmt.Errorf("publication path %q cannot modify Git metadata", value)
	}
	return cleaned, nil
}

func commitMetadata(repository apigen.GitRepositorySpec, publicationName string) (string, string, string) {
	name := "Stigmergy"
	email := "stigmergy@localhost"
	message := "Update inventory " + publicationName
	if repository.Commit == nil {
		return name, email, message
	}
	if repository.Commit.AuthorName != nil {
		name = *repository.Commit.AuthorName
	}
	if repository.Commit.AuthorEmail != nil {
		email = *repository.Commit.AuthorEmail
	}
	if repository.Commit.MessageTemplate != nil {
		message = strings.ReplaceAll(*repository.Commit.MessageTemplate, "{{ .Publication.metadata.name }}", publicationName)
	}
	return name, email, message
}
