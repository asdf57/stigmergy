package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"time"

	"github.com/asdf57/stigmergy/internal/api/registry"
)

// Build is the stable subset of Concourse's build response used by execution controllers.
type Build struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func (b Build) Terminal() bool {
	return b.Status == "succeeded" || b.Status == "failed" || b.Status == "errored" || b.Status == "aborted"
}

type ExecutionBackend interface {
	CheckResource(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string, string, map[string]string) error
	Trigger(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string) (Build, error)
	Builds(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string) ([]Build, error)
	Build(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string, int64) (Build, error)
	Abort(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, int64) error
}

// Register the requested immutable version before submitting a build; newly created
// Concourse resources otherwise discover only the latest Git commit.
func (b *FlyBackend) CheckResource(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, name, resourceName string, version map[string]string) error {
	args := []string{"check-resource", "--resource", name + "/" + resourceName}
	keys := make([]string, 0, len(version))
	for key := range version {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--from", key+":"+version[key])
	}
	_, err := b.execution(ctx, p, c, args...)
	return err
}

func providerTeam(provider registry.PipelineProvider) string {
	if provider.Spec.Team == "" {
		return "main"
	}
	return provider.Spec.Team
}
func (b *FlyBackend) Trigger(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, name string) (Build, error) {
	// Do not parse trigger-job's human output. Query this isolated job for its one build.
	if _, err := b.execution(ctx, p, c, "trigger-job", "-j", name+"/run"); err != nil {
		return Build{}, err
	}
	builds, err := b.Builds(ctx, p, c, name)
	if err != nil {
		return Build{}, err
	}
	if len(builds) != 1 || builds[0].ID < 1 {
		return Build{}, fmt.Errorf("submitted build is not yet unambiguously observable")
	}
	return builds[0], nil
}
func (b *FlyBackend) Builds(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, name string) ([]Build, error) {
	output, err := b.execution(ctx, p, c, "builds", "--job", name+"/run", "--count", "2", "--json")
	if err != nil {
		return nil, err
	}
	var builds []Build
	if err := json.Unmarshal(output, &builds); err != nil {
		return nil, fmt.Errorf("decode Concourse builds: %w", err)
	}
	return builds, nil
}
func (b *FlyBackend) Build(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, name string, id int64) (Build, error) {
	builds, err := b.Builds(ctx, p, c, name)
	if err != nil {
		return Build{}, err
	}
	for _, build := range builds {
		if build.ID == id {
			return build, nil
		}
	}
	return Build{}, fmt.Errorf("Concourse build %d is missing from its execution job; refusing to resubmit", id)
}
func (b *FlyBackend) Abort(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, id int64) error {
	_, err := b.execution(ctx, p, c, "abort-build", "--build", strconv.FormatInt(id, 10))
	return err
}

// Reuse native fly commands/authentication; no custom auth or curl runtime dependency.
func (b *FlyBackend) execution(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	home, err := os.MkdirTemp("", "stigmergy-fly-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	if err := b.login(ctx, home, p, c); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, b.executable, append([]string{"-t", "stigmergy"}, args...)...)
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("fly %s failed: %w", args[0], err)
	}
	return output, nil
}
