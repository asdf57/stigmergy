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
	Path            string
	Content         []byte
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
	repositoryPath, err := safeRepositoryPath(request.Path)
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
	filePath := filepath.Join(temporaryDirectory, filepath.FromSlash(repositoryPath))
	if existing, err := os.ReadFile(filePath); err == nil && bytes.Equal(existing, request.Content) {
		head, err := repository.Head()
		if err != nil {
			return PublishResult{}, fmt.Errorf("read unchanged Git revision: %w", err)
		}
		return PublishResult{Revision: head.Hash().String(), URL: request.Repository.Url, Changed: false}, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return PublishResult{}, fmt.Errorf("read existing inventory file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return PublishResult{}, fmt.Errorf("create inventory directory: %w", err)
	}
	if err := os.WriteFile(filePath, request.Content, 0o644); err != nil {
		return PublishResult{}, fmt.Errorf("write inventory file: %w", err)
	}
	if _, err := worktree.Add(repositoryPath); err != nil {
		return PublishResult{}, fmt.Errorf("stage inventory file: %w", err)
	}

	authorName, authorEmail, message := commitMetadata(request.Repository, request.PublicationName)
	commit, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{
		Name: authorName, Email: authorEmail, When: p.Now().UTC(),
	}})
	if err != nil {
		return PublishResult{}, fmt.Errorf("commit inventory file: %w", err)
	}
	refspec := config.RefSpec(fmt.Sprintf("%s:%s", branch, branch))
	if err := repository.PushContext(ctx, &git.PushOptions{Auth: auth, RefSpecs: []config.RefSpec{refspec}}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return PublishResult{}, fmt.Errorf("push inventory commit: %w", err)
	}
	return PublishResult{Revision: commit.String(), URL: request.Repository.Url, Changed: true}, nil
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
