package commandspipeline

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const (
	cleanupFinalizer   = "homelab.io/commands-pipeline-cleanup"
	ownerUIDAnnotation = "homelab.io/commands-pipeline-uid"
)

type Config struct {
	CommandRunnerImage     string
	PublicAPIURL           string
	AnsibleRolesRepository string
	AnsibleRolesRevision   string
	RunnerParameters       map[string]string
}

type Reconciler struct {
	store  store.Store
	config Config
	now    func() time.Time
}

func NewReconciler(resourceStore store.Store, config Config) *Reconciler {
	return &Reconciler{store: resourceStore, config: config, now: time.Now}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.CommandsPipelineResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get CommandsPipeline %q: %w", request.Name, err)
	}
	value, err := registry.CommandsPipelineResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode CommandsPipeline %q: %w", request.Name, err)
	}
	if value.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, value)
	}

	legacy, legacyErr := r.store.Get(ctx, registry.PipelineResource.Kind, value.Metadata.Name)
	if legacyErr == nil && legacy.Metadata.Annotations[ownerUIDAnnotation] == value.Metadata.UID {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhasePending, "LegacyPipelinePresent", "Retire the legacy Git-triggered Pipeline before enabling disposable Commands", nil)
	}
	if legacyErr != nil && !errors.Is(legacyErr, store.ErrNotFound) {
		return legacyErr
	}
	if message := r.configError(); message != "" {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhasePending, "ConfigurationUnavailable", message, nil)
	}
	repository, group, provider, keyPair, phase, reason, message, err := r.resolve(ctx, value)
	if err != nil {
		return err
	}
	if phase != "" {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhase(phase), reason, message, nil)
	}
	_, _ = group, provider
	if _, err := r.render(value, repository, keyPair, "validation", "validation/run.sh", "validation"); err != nil {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhaseFailed, "RunnerConfigurationInvalid", err.Error(), nil)
	}
	if (value.Spec.Schedule == nil) != (value.Spec.CommandTemplate == nil) {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhaseFailed, "InvalidSchedule", "schedule and commandTemplate must be supplied together", nil)
	}
	if err := r.schedule(ctx, &value); err != nil {
		return err
	}
	return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhaseReady, "ExecutorReady", "Reusable command executor is Ready", nil)
}

func (r *Reconciler) configError() string {
	missing := make([]string, 0)
	if strings.TrimSpace(r.config.CommandRunnerImage) == "" {
		missing = append(missing, "COMMAND_RUNNER_IMAGE")
	}
	if strings.TrimSpace(r.config.PublicAPIURL) == "" {
		missing = append(missing, "PUBLIC_API_URL")
	}
	if strings.TrimSpace(r.config.AnsibleRolesRepository) == "" {
		missing = append(missing, "ANSIBLE_ROLES_REPOSITORY")
	}
	if strings.TrimSpace(r.config.AnsibleRolesRevision) == "" {
		missing = append(missing, "ANSIBLE_ROLES_REVISION")
	}
	if len(missing) == 0 {
		return ""
	}
	return "controller configuration is missing: " + strings.Join(missing, ", ")
}

func (r *Reconciler) resolve(ctx context.Context, value registry.CommandsPipeline) (registry.GitRepository, registry.InventoryCaptureGroup, registry.PipelineProvider, registry.SSHKeyPair, string, string, string, error) {
	get := func(kind, name string) (resource.Resource, error) { return r.store.Get(ctx, kind, name) }
	raw, err := get(registry.GitRepositoryResource.Kind, value.Spec.CommandsRepositoryRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return registry.GitRepository{}, registry.InventoryCaptureGroup{}, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Pending", "RepositoryNotFound", fmt.Sprintf("GitRepository %q does not exist", value.Spec.CommandsRepositoryRef.Name), nil
	}
	if err != nil {
		return registry.GitRepository{}, registry.InventoryCaptureGroup{}, registry.PipelineProvider{}, registry.SSHKeyPair{}, "", "", "", err
	}
	repository, err := registry.GitRepositoryResource.Decode(raw)
	if err != nil {
		return registry.GitRepository{}, registry.InventoryCaptureGroup{}, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Failed", "RepositoryInvalid", err.Error(), nil
	}
	raw, err = get(registry.InventoryCaptureGroupResource.Kind, value.Spec.InventoryCaptureGroupRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return repository, registry.InventoryCaptureGroup{}, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Pending", "CaptureGroupNotFound", fmt.Sprintf("InventoryCaptureGroup %q does not exist", value.Spec.InventoryCaptureGroupRef.Name), nil
	}
	if err != nil {
		return repository, registry.InventoryCaptureGroup{}, registry.PipelineProvider{}, registry.SSHKeyPair{}, "", "", "", err
	}
	group, err := registry.InventoryCaptureGroupResource.Decode(raw)
	if err != nil {
		return repository, registry.InventoryCaptureGroup{}, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Failed", "CaptureGroupInvalid", err.Error(), nil
	}
	if group.Status == nil || group.Status.Phase == nil || *group.Status.Phase != "Ready" || group.Status.ObservedGeneration == nil || *group.Status.ObservedGeneration != group.Metadata.Generation {
		return repository, group, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Pending", "CaptureGroupNotReady", fmt.Sprintf("InventoryCaptureGroup %q is not Ready", group.Metadata.Name), nil
	}

	raw, err = get(registry.PipelineProviderResource.Kind, value.Spec.PipelineProviderRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return repository, group, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Pending", "ProviderNotFound", fmt.Sprintf("PipelineProvider %q does not exist", value.Spec.PipelineProviderRef.Name), nil
	}
	if err != nil {
		return repository, group, registry.PipelineProvider{}, registry.SSHKeyPair{}, "", "", "", err
	}
	provider, err := registry.PipelineProviderResource.Decode(raw)
	if err != nil {
		return repository, group, registry.PipelineProvider{}, registry.SSHKeyPair{}, "Failed", "ProviderInvalid", err.Error(), nil
	}
	if provider.Status == nil || provider.Status.Phase == nil || *provider.Status.Phase != apigen.PipelineProviderStatusPhaseReady || provider.Status.ObservedGeneration == nil || *provider.Status.ObservedGeneration != provider.Metadata.Generation {
		return repository, group, provider, registry.SSHKeyPair{}, "Pending", "ProviderNotReady", fmt.Sprintf("PipelineProvider %q is not Ready", provider.Metadata.Name), nil
	}

	if repository.Spec.Authentication == nil || repository.Spec.Authentication.SshKeyPairRef == "" {
		return repository, group, provider, registry.SSHKeyPair{}, "", "", "", nil
	}
	raw, err = get(registry.SSHKeyPairResource.Kind, repository.Spec.Authentication.SshKeyPairRef)
	if errors.Is(err, store.ErrNotFound) {
		return repository, group, provider, registry.SSHKeyPair{}, "Pending", "SSHKeyPairNotFound", fmt.Sprintf("SSHKeyPair %q does not exist", repository.Spec.Authentication.SshKeyPairRef), nil
	}
	if err != nil {
		return repository, group, provider, registry.SSHKeyPair{}, "", "", "", err
	}
	keyPair, err := registry.SSHKeyPairResource.Decode(raw)
	if err != nil {
		return repository, group, provider, registry.SSHKeyPair{}, "Failed", "SSHKeyPairInvalid", err.Error(), nil
	}
	if keyPair.Status == nil || keyPair.Status.Phase == nil || *keyPair.Status.Phase != apigen.SSHKeyPairStatusPhaseReady || keyPair.Status.ObservedGeneration == nil || *keyPair.Status.ObservedGeneration != keyPair.Metadata.Generation {
		return repository, group, provider, keyPair, "Pending", "SSHKeyPairNotReady", fmt.Sprintf("SSHKeyPair %q is not Ready", keyPair.Metadata.Name), nil
	}
	return repository, group, provider, keyPair, "", "", "", nil
}

// Prepare resolves and snapshots reusable settings for a single execution.
func (r *Reconciler) Prepare(ctx context.Context, value registry.CommandsPipeline, branch, commandPath, revision string) (registry.GitRepository, registry.PipelineProvider, string, error) {
	if message := r.configError(); message != "" {
		return registry.GitRepository{}, registry.PipelineProvider{}, "", fmt.Errorf("%s", message)
	}
	repo, _, provider, key, phase, _, message, err := r.resolve(ctx, value)
	if err != nil {
		return repo, provider, "", err
	}
	if phase != "" {
		return repo, provider, "", fmt.Errorf("%s", message)
	}
	definition, err := r.render(value, repo, key, branch, commandPath, revision)
	return repo, provider, definition, err
}

func (r *Reconciler) render(value registry.CommandsPipeline, repository registry.GitRepository, keyPair registry.SSHKeyPair, branch, commandPath, revision string) (string, error) {
	imageRepository, imageTag := splitImage(r.config.CommandRunnerImage)
	resourceSource := map[string]any{
		"uri": repository.Spec.Url, "branch": branch,
	}
	if repository.Spec.Authentication != nil && repository.Spec.Authentication.SshKeyPairRef != "" {
		resourceSource["private_key"] = "((" + keyPair.Spec.Path + ".privateKey))"
	}
	resourceConfig := map[string]any{
		"name": "commands", "type": "git", "check_every": "1m",
		"source": resourceSource,
	}
	taskConfig := map[string]any{
		"platform": "linux",
		"image_resource": map[string]any{
			"type":   "registry-image",
			"source": map[string]any{"repository": imageRepository, "tag": imageTag},
		},
		"inputs": []any{map[string]any{"name": "commands"}},
		"params": map[string]any{
			"CONTAINER_MODE": "normal", "INVENTORY_CAPTURE_GROUP": value.Spec.InventoryCaptureGroupRef.Name,
			"STIGMERGY_API_URL": r.config.PublicAPIURL, "GIT_ANSIBLE_ROLES_REPO": r.config.AnsibleRolesRepository,
			"GIT_ANSIBLE_ROLES_REF": r.config.AnsibleRolesRevision, "COMMAND_FILE": commandPath,
		},
		"run": map[string]any{
			"path": "/bin/bash",
			"user": "keiichi",
			"args": []string{"--login", "-c", `set -euo pipefail
command_file="$PWD/commands/$COMMAND_FILE"
test -f "$command_file"
exec /bin/bash "$command_file"`},
		},
	}
	parameters := taskConfig["params"].(map[string]any)
	for name, value := range r.config.RunnerParameters {
		if _, reserved := parameters[name]; reserved {
			return "", fmt.Errorf("runner parameter %q overrides controller configuration", name)
		}
		parameters[name] = value
	}
	resources := []any{resourceConfig}
	plan := []any{map[string]any{"get": "commands", "version": map[string]any{"ref": revision}}}
	plan = append(plan, map[string]any{"task": "run-command", "config": taskConfig})
	config := map[string]any{
		"resources": resources,
		"jobs": []any{map[string]any{
			"name": "run", "serial": true,
			"plan": plan,
		}},
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshal Concourse pipeline: %w", err)
	}
	return string(data), nil
}

func splitImage(image string) (string, string) {
	lastSlash, lastColon := strings.LastIndex(image, "/"), strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:]
	}
	return image, "latest"
}

func (r *Reconciler) setStatus(ctx context.Context, value registry.CommandsPipeline, phase apigen.CommandsPipelineStatusPhase, reason, message string, reference *apigen.ResourceReference) error {
	generation := value.Metadata.Generation
	conditionStatus := apigen.CommandsPipelineConditionStatusFalse
	if phase == apigen.CommandsPipelineStatusPhaseReady {
		conditionStatus = apigen.CommandsPipelineConditionStatusTrue
	}
	conditions := []apigen.CommandsPipelineCondition{{Type: "Ready", Status: conditionStatus, Reason: reason, Message: &message, ObservedGeneration: &generation}}
	status := &apigen.CommandsPipelineStatus{Phase: &phase, ObservedGeneration: &generation, LastScheduledAt: lastScheduledAt(value), Conditions: &conditions}
	if resource.EqualJSON(value.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	encoded, err := registry.CommandsPipelineResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, value.Kind, value.Metadata.Name, encoded, revision)
	return err
}

func lastScheduledAt(value registry.CommandsPipeline) *time.Time {
	if value.Status != nil {
		return value.Status.LastScheduledAt
	}
	return nil
}

// No backfill and no overlap for scheduled runs. Ad-hoc Commands are independent.
func (r *Reconciler) schedule(ctx context.Context, value *registry.CommandsPipeline) error {
	if value.Spec.Schedule == nil {
		return nil
	}
	interval, err := time.ParseDuration(*value.Spec.Schedule)
	if err != nil || interval < time.Second {
		return fmt.Errorf("invalid schedule interval")
	}
	now := r.now().UTC()
	slot := time.Unix((now.Unix()/int64(interval/time.Second))*int64(interval/time.Second), 0).UTC()
	if previous := lastScheduledAt(*value); previous != nil && !previous.Before(slot) {
		return nil
	}
	items, err := r.store.List(ctx, registry.CommandResource.Kind)
	if err != nil {
		return err
	}
	active := false
	for _, raw := range items.Items {
		if raw.Metadata.Annotations["homelab.io/scheduled-executor-uid"] != value.Metadata.UID {
			continue
		}
		command, err := registry.CommandResource.Decode(raw)
		if err != nil {
			return err
		}
		if command.Status == nil || command.Status.Phase == nil || (*command.Status.Phase != apigen.CommandStatusPhaseSucceeded && *command.Status.Phase != apigen.CommandStatusPhaseFailed) {
			active = true
		}
	}
	if value.Status == nil {
		value.Status = &apigen.CommandsPipelineStatus{}
	}
	value.Status.LastScheduledAt = &slot
	// Claim this slot before creating: no duplicate request after a crash plus TTL deletion.
	// A crash between this CAS and creation may skip the slot; missed slots are not backfilled.
	status, err := registry.CommandsPipelineResource.EncodeStatus(value.Status)
	if err != nil {
		return err
	}
	version, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	updated, err := r.store.UpdateStatus(ctx, value.Kind, value.Metadata.Name, status, version)
	if err != nil {
		return err
	}
	*value, err = registry.CommandsPipelineResource.Decode(updated)
	if err != nil {
		return err
	}
	if !active {
		name := fmt.Sprintf("scheduled-%x-%d", sha256.Sum256([]byte(value.Metadata.UID)), slot.Unix())
		uid := value.Metadata.UID
		command := registry.NewCommand(resource.Metadata{Name: name, Finalizers: append([]string(nil), registry.CommandResource.DefaultFinalizers...), Annotations: map[string]string{"homelab.io/scheduled-executor-uid": uid}}, apigen.CommandSpec{CommandsPipelineRef: apigen.CommandExecutorReference{Name: value.Metadata.Name, Uid: &uid}, Script: value.Spec.CommandTemplate.Script, TtlSecondsAfterFinished: value.Spec.CommandTemplate.TtlSecondsAfterFinished})
		encoded, err := command.Encode()
		if err != nil {
			return err
		}
		if _, err := r.store.Create(ctx, encoded); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return err
			}
			existing, getErr := r.store.Get(ctx, command.Kind, name)
			if getErr != nil {
				return getErr
			}
			if existing.Metadata.Annotations["homelab.io/scheduled-executor-uid"] != uid || !resource.EqualJSON(existing.Spec, encoded.Spec) {
				return fmt.Errorf("scheduled Command ownership conflict")
			}
		}
	}
	return nil
}

func (r *Reconciler) finalize(ctx context.Context, value registry.CommandsPipeline) error {
	commands, err := r.store.List(ctx, registry.CommandResource.Kind)
	if err != nil {
		return err
	}
	for _, raw := range commands.Items {
		command, err := registry.CommandResource.Decode(raw)
		if err != nil {
			return err
		}
		if command.Spec.CommandsPipelineRef.Name == value.Metadata.Name {
			return fmt.Errorf("CommandsPipeline is still referenced by Command %q; delete its Commands first", command.Metadata.Name)
		}
	}
	// Legacy child pipelines must be retired explicitly before migration.
	raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, value.Metadata.Name)
	if err == nil && raw.Metadata.Annotations[ownerUIDAnnotation] == value.Metadata.UID {
		return fmt.Errorf("retire legacy Pipeline %q before deleting its executor", raw.Metadata.Name)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	finalizers := make([]string, 0, len(value.Metadata.Finalizers))
	for _, item := range value.Metadata.Finalizers {
		if item != cleanupFinalizer {
			finalizers = append(finalizers, item)
		}
	}
	value.Metadata.Finalizers = finalizers
	encoded, err := value.Encode()
	if err != nil {
		return err
	}
	version, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, version)
	return err
}

func (r *Reconciler) requestsMatching(ctx context.Context, match func(registry.CommandsPipeline) bool) ([]controller.Request, error) {
	items, err := r.store.List(ctx, registry.CommandsPipelineResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := make([]controller.Request, 0)
	for _, raw := range items.Items {
		value, err := registry.CommandsPipelineResource.Decode(raw)
		if err != nil {
			return nil, err
		}
		if match(value) {
			requests = append(requests, controller.Request{Kind: value.Kind, Name: value.Metadata.Name})
		}
	}
	return requests, nil
}
func (r *Reconciler) RequestsForRepository(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(value registry.CommandsPipeline) bool {
		return value.Spec.CommandsRepositoryRef.Name == request.Name
	})
}
func (r *Reconciler) RequestsForCaptureGroup(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(value registry.CommandsPipeline) bool {
		return value.Spec.InventoryCaptureGroupRef.Name == request.Name
	})
}
func (r *Reconciler) RequestsForProvider(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(value registry.CommandsPipeline) bool { return value.Spec.PipelineProviderRef.Name == request.Name })
}
func (r *Reconciler) RequestsForPipeline(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	if _, err := r.store.Get(ctx, registry.CommandsPipelineResource.Kind, request.Name); errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return []controller.Request{{Kind: registry.CommandsPipelineResource.Kind, Name: request.Name}}, nil
}
func (r *Reconciler) RequestsForSSHKeyPair(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(value registry.CommandsPipeline) bool {
		raw, err := r.store.Get(ctx, registry.GitRepositoryResource.Kind, value.Spec.CommandsRepositoryRef.Name)
		if err != nil {
			return false
		}
		repository, err := registry.GitRepositoryResource.Decode(raw)
		return err == nil && repository.Spec.Authentication != nil && repository.Spec.Authentication.SshKeyPairRef == request.Name
	})
}
