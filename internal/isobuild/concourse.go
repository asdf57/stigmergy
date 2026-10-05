// Package isobuild renders build recipes without performing provider API calls.
package isobuild

import (
	"encoding/json"
	"fmt"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"go.yaml.in/yaml/v3"
	"strings"
)

type Config struct {
	BuilderRepository      string
	BuilderBranch          string
	DaemonRepository       string
	DaemonBranch           string
	PublicAPIURL           string
	ArtifactBaseURL        string
	UploadPasswordVariable string
}

// Input content is compared directly against manifests, independent of commits
// elsewhere on a shared branch. JSON is a valid YAML image.yaml document.
func Inputs(image registry.ISO, digest string, config Config) (string, error) {
	if image.Spec.Distribution == "debian" && image.Spec.Version != "trixie" || image.Spec.Distribution == "arch" && image.Spec.Version != "rolling" {
		return "", fmt.Errorf("supported recipes are Debian trixie and Arch rolling")
	}
	value := map[string]any{
		"isoUid": image.Metadata.UID, "distribution": image.Spec.Distribution, "version": image.Spec.Version,
		"architecture": image.Spec.Architecture, "bootMode": image.Spec.BootMode, "trustBundleDigest": digest, "recipeVersion": "1",
		"apiEndpoint": config.PublicAPIURL, "artifactBaseURL": strings.TrimRight(config.ArtifactBaseURL, "/"),
		"builderRepository": config.BuilderRepository, "builderBranch": config.BuilderBranch,
		"daemonRepository": config.DaemonRepository, "daemonBranch": config.DaemonBranch,
	}
	data, err := json.MarshalIndent(value, "", "  ")
	return string(data) + "\n", err
}

func RenderConcourse(image registry.ISO, repository registry.GitRepository, gitKeyPath string, config Config) (string, error) {
	for _, source := range []string{config.BuilderRepository, config.DaemonRepository} {
		if !strings.HasPrefix(source, "https://") {
			return "", fmt.Errorf("builder and daemon sources must use public HTTPS Git URLs")
		}
	}
	for _, value := range []string{config.BuilderRepository, config.BuilderBranch, config.DaemonRepository, config.DaemonBranch, config.PublicAPIURL, config.ArtifactBaseURL, config.UploadPasswordVariable} {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("ISO build deployment configuration is incomplete")
		}
	}
	inputSource := map[string]any{"uri": repository.Spec.Url, "branch": image.Spec.BuildInputs.Branch, "paths": []string{image.Spec.BuildInputs.Path + "/**"}}
	if gitKeyPath != "" {
		inputSource["private_key"] = "((" + gitKeyPath + ".privateKey))"
	}
	gitResource := func(name, uri, branch string, paths []string) map[string]any {
		source := map[string]any{"uri": uri, "branch": branch}
		if paths != nil {
			source["paths"] = paths
		}
		return map[string]any{"name": name, "type": "git", "source": source, "check_every": "1m"}
	}
	task := func(repository, tag, script string, inputs, outputs []string, params map[string]any) map[string]any {
		in, out := []any{}, []any{}
		for _, name := range inputs {
			in = append(in, map[string]any{"name": name})
		}
		for _, name := range outputs {
			out = append(out, map[string]any{"name": name})
		}
		return map[string]any{"platform": "linux", "image_resource": map[string]any{"type": "registry-image", "source": map[string]any{"repository": repository, "tag": tag}}, "inputs": in, "outputs": out, "params": params, "run": map[string]any{"path": script}}
	}
	compile := task("golang", "1.26", "/bin/sh", []string{"homelabd"}, []string{"homelabd-bin"}, nil)
	compile["run"] = map[string]any{"path": "/bin/sh", "args": []string{"-ec", "cd homelabd; CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ../homelabd-bin/homelabd ."}}
	builderRepo, builderTag := "archlinux", "latest"
	if image.Spec.Distribution == "debian" {
		builderRepo, builderTag = "debian", "trixie"
	}
	build := task(builderRepo, builderTag, "/bin/bash", []string{"image-inputs", "builder", "homelabd", "homelabd-bin"}, []string{"image-output"}, map[string]any{"IMAGE_INPUT_PATH": image.Spec.BuildInputs.Path})
	build["run"] = map[string]any{"path": "/bin/bash", "args": []string{"builder/roles/os/files/ci/build-image.sh"}}
	publish := task("python", "3.13-alpine", "python3", []string{"builder", "image-output"}, nil, map[string]any{"FILE_REGISTRY_PASSWORD": "((" + config.UploadPasswordVariable + "))"})
	publish["run"] = map[string]any{"path": "python3", "args": []string{"builder/roles/os/files/ci/publish.py"}}
	pipeline := map[string]any{
		"resources": []any{
			map[string]any{"name": "image-inputs", "type": "git", "check_every": "1m", "source": inputSource},
			gitResource("builder", config.BuilderRepository, config.BuilderBranch, []string{"roles/os/files/**"}),
			gitResource("homelabd", config.DaemonRepository, config.DaemonBranch, nil),
		},
		"jobs": []any{map[string]any{"name": "build", "serial": true, "plan": []any{
			map[string]any{"in_parallel": []any{map[string]any{"get": "image-inputs", "trigger": true}, map[string]any{"get": "builder", "trigger": true}, map[string]any{"get": "homelabd", "trigger": true}}},
			map[string]any{"task": "compile-homelabd", "config": compile},
			map[string]any{"task": "build-image", "privileged": true, "config": build},
			map[string]any{"task": "publish-image", "config": publish},
		}}},
	}
	data, err := yaml.Marshal(pipeline)
	return string(data), err
}
