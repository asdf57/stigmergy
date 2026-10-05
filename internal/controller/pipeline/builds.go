package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asdf57/stigmergy/internal/api/registry"
	"go.yaml.in/yaml/v3"
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
	output, err := b.execution(ctx, p, c, "api", "POST", "/api/v1/teams/"+url.PathEscape(providerTeam(p))+"/pipelines/"+url.PathEscape(name)+"/jobs/run/builds")
	if err != nil {
		return Build{}, err
	}
	var build Build
	if err := json.Unmarshal(output, &build); err != nil || build.ID < 1 {
		return Build{}, fmt.Errorf("submission did not return a valid build ID")
	}
	return build, nil
}
func (b *FlyBackend) Builds(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, name string) ([]Build, error) {
	output, err := b.execution(ctx, p, c, "builds", "--job", name+"/run", "--count", "100", "--json")
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
	output, err := b.execution(ctx, p, c, "api", "GET", "/api/v1/builds/"+strconv.FormatInt(id, 10))
	if err != nil {
		return Build{}, err
	}
	var build struct {
		Build        `yaml:",inline"`
		PipelineName string `json:"pipeline_name"`
		JobName      string `json:"job_name"`
		TeamName     string `json:"team_name"`
	}
	if err := json.Unmarshal(output, &build); err != nil {
		return Build{}, err
	}
	if build.ID != id || build.PipelineName != name || build.JobName != "run" || build.TeamName != providerTeam(p) {
		return Build{}, fmt.Errorf("Concourse build does not belong to the requested executor")
	}
	return build.Build, nil
}
func (b *FlyBackend) Abort(ctx context.Context, p registry.PipelineProvider, c registry.UsernamePasswordCredential, id int64) error {
	_, err := b.execution(ctx, p, c, "abort-build", "--build", strconv.FormatInt(id, 10))
	return err
}

// Reuse fly's standard login and issued bearer token. JSON build endpoints give
// an unambiguous ID and direct lookup even when a shared job has long history.
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
	if args[0] == "api" {
		data, err := os.ReadFile(filepath.Join(home, ".flyrc"))
		if err != nil {
			return nil, err
		}
		var rc struct {
			Targets map[string]struct {
				Token struct {
					Type  string `yaml:"type"`
					Value string `yaml:"value"`
				} `yaml:"token"`
			} `yaml:"targets"`
		}
		if err := yaml.Unmarshal(data, &rc); err != nil {
			return nil, fmt.Errorf("decode fly authentication configuration")
		}
		token := rc.Targets["stigmergy"].Token
		if token.Type == "" || token.Value == "" {
			return nil, fmt.Errorf("fly login returned no token")
		}
		request, err := http.NewRequestWithContext(ctx, args[1], strings.TrimRight(p.Spec.Url, "/")+args[2], nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", token.Type+" "+token.Value)
		client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("Concourse build request failed: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("Concourse build endpoint returned HTTP %d", response.StatusCode)
		}
		return io.ReadAll(io.LimitReader(response.Body, 1<<20))
	}
	command := exec.CommandContext(ctx, b.executable, append([]string{"-t", "stigmergy"}, args...)...)
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("fly %s failed: %w", args[0], err)
	}
	return output, nil
}
