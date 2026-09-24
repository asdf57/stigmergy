package commandspipeline

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const (
	cleanupFinalizer   = "homelab.io/commands-pipeline-cleanup"
	pipelineFinalizer  = "homelab.io/pipeline-cleanup"
	ownerUIDAnnotation = "homelab.io/commands-pipeline-uid"
)

var errOwnershipConflict = errors.New("Pipeline ownership conflict")

type Config struct {
	CommandRunnerImage     string
	PublicAPIURL           string
	AnsibleRolesRepository string
	AnsibleRolesRevision   string
}

type Reconciler struct {
	store  store.Store
	config Config
}

func NewReconciler(resourceStore store.Store, config Config) *Reconciler {
	return &Reconciler{store: resourceStore, config: config}
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
	commandPath, err := safeCommandPath(value)
	if err != nil {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhaseFailed, "InvalidCommandPath", err.Error(), nil)
	}
	definition, err := r.render(value, repository, keyPair, commandPath)
	if err != nil {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhaseFailed, "RenderFailed", err.Error(), nil)
	}
	child, changed, err := r.ensurePipeline(ctx, value, provider, definition)
	if err != nil {
		phase, reason := apigen.CommandsPipelineStatusPhaseFailed, "PipelineProvisioningFailed"
		if errors.Is(err, errOwnershipConflict) {
			phase, reason = apigen.CommandsPipelineStatusPhaseConflict, "PipelineOwnershipConflict"
		}
		return r.setStatus(ctx, value, phase, reason, err.Error(), nil)
	}
	reference := &apigen.ResourceReference{Name: child.Metadata.Name, Uid: child.Metadata.UID}
	if changed || !childReady(child) {
		return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhasePending, "PipelinePending", fmt.Sprintf("Pipeline %q is waiting for reconciliation", child.Metadata.Name), reference)
	}
	_ = group
	return r.setStatus(ctx, value, apigen.CommandsPipelineStatusPhaseReady, "CommandsPipelineReady", "The commands pipeline is configured and Ready", reference)
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

func safeCommandPath(value registry.CommandsPipeline) (string, error) {
	commandPath := value.Spec.InventoryCaptureGroupRef.Name + ".sh"
	if value.Spec.CommandPath != nil {
		commandPath = strings.TrimSpace(*value.Spec.CommandPath)
	}
	cleaned := path.Clean(commandPath)
	if commandPath == "" || path.IsAbs(commandPath) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != commandPath {
		return "", fmt.Errorf("commandPath %q must be a clean relative path", commandPath)
	}
	return cleaned, nil
}

func (r *Reconciler) render(value registry.CommandsPipeline, repository registry.GitRepository, keyPair registry.SSHKeyPair, commandPath string) (string, error) {
	imageRepository, imageTag := splitImage(r.config.CommandRunnerImage)
	resourceSource := map[string]any{
		"uri": repository.Spec.Url, "branch": repository.Spec.Branch,
		"paths": []string{commandPath},
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
	config := map[string]any{
		"resources": []any{resourceConfig},
		"jobs": []any{map[string]any{
			"name": "run", "serial": true,
			"plan": []any{
				map[string]any{"get": "commands", "trigger": true},
				map[string]any{"task": "run-command", "privileged": true, "config": taskConfig},
			},
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

func (r *Reconciler) ensurePipeline(ctx context.Context, owner registry.CommandsPipeline, provider registry.PipelineProvider, definition string) (registry.Pipeline, bool, error) {
	name := owner.Metadata.Name
	raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, name)
	if errors.Is(err, store.ErrNotFound) {
		desired := desiredPipeline(owner, provider, definition, resource.Metadata{Name: name, Finalizers: []string{pipelineFinalizer}, Annotations: map[string]string{ownerUIDAnnotation: owner.Metadata.UID}})
		encoded, err := desired.Encode()
		if err != nil {
			return registry.Pipeline{}, false, err
		}
		created, err := r.store.Create(ctx, encoded)
		if errors.Is(err, store.ErrConflict) {
			return r.ensurePipeline(ctx, owner, provider, definition)
		}
		if err != nil {
			return registry.Pipeline{}, false, err
		}
		child, err := registry.PipelineResource.Decode(created)
		return child, true, err
	}
	if err != nil {
		return registry.Pipeline{}, false, err
	}
	child, err := registry.PipelineResource.Decode(raw)
	if err != nil {
		return registry.Pipeline{}, false, err
	}
	if child.Metadata.Annotations[ownerUIDAnnotation] != owner.Metadata.UID {
		return child, false, fmt.Errorf("%w: Pipeline %q is owned by another CommandsPipeline", errOwnershipConflict, name)
	}
	if child.Metadata.DeletionTimestamp != nil {
		return child, false, fmt.Errorf("Pipeline %q is terminating", name)
	}
	desired := desiredPipeline(owner, provider, definition, child.Metadata)
	if resource.EqualJSON(child.Spec, desired.Spec) && slices.Contains(child.Metadata.Finalizers, pipelineFinalizer) {
		return child, false, nil
	}
	if !slices.Contains(desired.Metadata.Finalizers, pipelineFinalizer) {
		desired.Metadata.Finalizers = append(desired.Metadata.Finalizers, pipelineFinalizer)
	}
	encoded, err := desired.Encode()
	if err != nil {
		return registry.Pipeline{}, false, err
	}
	revision, err := strconv.ParseInt(child.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return registry.Pipeline{}, false, err
	}
	updated, err := r.store.Update(ctx, encoded, revision)
	if err != nil {
		return registry.Pipeline{}, false, err
	}
	child, err = registry.PipelineResource.Decode(updated)
	return child, true, err
}

func desiredPipeline(owner registry.CommandsPipeline, provider registry.PipelineProvider, definition string, metadata resource.Metadata) registry.Pipeline {
	return registry.NewPipeline(metadata, apigen.PipelineSpec{ProviderRef: apigen.PipelineProviderReference{Name: provider.Metadata.Name}, ExternalName: "commands-" + owner.Metadata.Name, Definition: apigen.PipelineDefinition{Format: apigen.PipelineDefinitionFormatConcourse, Data: definition}})
}

func childReady(value registry.Pipeline) bool {
	return value.Status != nil && value.Status.Phase != nil && *value.Status.Phase == apigen.PipelineStatusPhaseReady && value.Status.ObservedGeneration != nil && *value.Status.ObservedGeneration == value.Metadata.Generation
}

func (r *Reconciler) setStatus(ctx context.Context, value registry.CommandsPipeline, phase apigen.CommandsPipelineStatusPhase, reason, message string, reference *apigen.ResourceReference) error {
	generation := value.Metadata.Generation
	conditionStatus := apigen.CommandsPipelineConditionStatusFalse
	if phase == apigen.CommandsPipelineStatusPhaseReady {
		conditionStatus = apigen.CommandsPipelineConditionStatusTrue
	}
	conditions := []apigen.CommandsPipelineCondition{{Type: "Ready", Status: conditionStatus, Reason: reason, Message: &message, ObservedGeneration: &generation}}
	status := &apigen.CommandsPipelineStatus{Phase: &phase, ObservedGeneration: &generation, PipelineRef: reference, Conditions: &conditions}
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

func (r *Reconciler) finalize(ctx context.Context, value registry.CommandsPipeline) error {
	if !slices.Contains(value.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, value.Metadata.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.removeFinalizer(ctx, value)
	}
	if err != nil {
		return err
	}
	child, err := registry.PipelineResource.Decode(raw)
	if err != nil {
		return err
	}
	if child.Metadata.Annotations[ownerUIDAnnotation] != value.Metadata.UID {
		return fmt.Errorf("%w: refusing to delete Pipeline %q", errOwnershipConflict, child.Metadata.Name)
	}
	if child.Metadata.DeletionTimestamp == nil {
		revision, err := strconv.ParseInt(child.Metadata.ResourceVersion, 10, 64)
		if err != nil {
			return err
		}
		if err := r.store.Delete(ctx, child.Kind, child.Metadata.Name, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) removeFinalizer(ctx context.Context, value registry.CommandsPipeline) error {
	value.Metadata.Finalizers = slices.DeleteFunc(value.Metadata.Finalizers, func(item string) bool { return item == cleanupFinalizer })
	encoded, err := value.Encode()
	if err != nil {
		return err
	}
	revision, err := strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, revision)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
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
func (r *Reconciler) RequestsForPipeline(_ context.Context, request controller.Request) ([]controller.Request, error) {
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
