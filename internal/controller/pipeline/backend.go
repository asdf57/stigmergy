package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/asdf57/stigmergy/internal/api/registry"
)

type Backend interface {
	Apply(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string, string) error
	Delete(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string) error
}

type FlyBackend struct{ executable string }

func NewFlyBackend() *FlyBackend { return &FlyBackend{executable: "fly"} }

func (b *FlyBackend) Apply(ctx context.Context, provider registry.PipelineProvider, credential registry.UsernamePasswordCredential, name, definition string) error {
	home, err := os.MkdirTemp("", "stigmergy-fly-")
	if err != nil {
		return fmt.Errorf("create fly workspace: %w", err)
	}
	defer os.RemoveAll(home)
	configPath := filepath.Join(home, "pipeline.yml")
	if err := os.WriteFile(configPath, []byte(definition), 0o600); err != nil {
		return fmt.Errorf("write pipeline definition: %w", err)
	}
	if err := b.login(ctx, home, provider, credential); err != nil {
		return err
	}
	if err := b.run(ctx, home, "set-pipeline", "-t", "stigmergy", "-p", name, "--non-interactive", "-c", configPath); err != nil {
		return err
	}
	return b.run(ctx, home, "unpause-pipeline", "-t", "stigmergy", "-p", name)
}

func (b *FlyBackend) Delete(ctx context.Context, provider registry.PipelineProvider, credential registry.UsernamePasswordCredential, name string) error {
	home, err := os.MkdirTemp("", "stigmergy-fly-")
	if err != nil {
		return fmt.Errorf("create fly workspace: %w", err)
	}
	defer os.RemoveAll(home)
	if err := b.login(ctx, home, provider, credential); err != nil {
		return err
	}
	err = b.run(ctx, home, "destroy-pipeline", "-t", "stigmergy", "-p", name, "--non-interactive")
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "pipeline not found") {
		return nil
	}
	return err
}

func (b *FlyBackend) login(ctx context.Context, home string, provider registry.PipelineProvider, credential registry.UsernamePasswordCredential) error {
	return b.run(ctx, home, "login", "-t", "stigmergy", "-c", provider.Spec.Url, "-n", provider.Spec.Team, "-u", credential.Spec.Username, "-p", credential.Spec.Password)
}

func (b *FlyBackend) run(ctx context.Context, home string, args ...string) error {
	command := exec.CommandContext(ctx, b.executable, args...)
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return fmt.Errorf("fly %s: %w", args[0], err)
		}
		return fmt.Errorf("fly %s: %w: %s", args[0], err, message)
	}
	return nil
}
