package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
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

type Reconciler struct {
	store     store.Store
	publisher Publisher
	now       func() time.Time
}

func NewReconciler(store store.Store) *Reconciler {
	return &Reconciler{store: store, publisher: NewGitPublisher(store), now: time.Now}
}

func NewReconcilerWithPublisher(store store.Store, publisher Publisher) *Reconciler {
	return &Reconciler{store: store, publisher: publisher, now: time.Now}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.InventoryPublicationResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get InventoryPublication %q: %w", request.Name, err)
	}
	publication, err := registry.InventoryPublicationResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode InventoryPublication %q: %w", request.Name, err)
	}

	groupRaw, err := r.store.Get(ctx, registry.InventoryCaptureGroupResource.Kind, publication.Spec.InventoryCaptureGroupRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.updateFailure(ctx, publication, "Pending", "CaptureGroupNotFound", fmt.Sprintf("InventoryCaptureGroup %q does not exist", publication.Spec.InventoryCaptureGroupRef.Name))
	}
	if err != nil {
		return fmt.Errorf("get InventoryCaptureGroup %q: %w", publication.Spec.InventoryCaptureGroupRef.Name, err)
	}
	group, err := registry.InventoryCaptureGroupResource.Decode(groupRaw)
	if err != nil {
		return fmt.Errorf("decode InventoryCaptureGroup %q: %w", publication.Spec.InventoryCaptureGroupRef.Name, err)
	}
	if publicationRequiresReady(publication.Spec) && (group.Status == nil || group.Status.Phase == nil || *group.Status.Phase != "Ready") {
		return r.updateFailure(ctx, publication, "Pending", "InventoryNotReady", fmt.Sprintf("InventoryCaptureGroup %q is not Ready", group.Metadata.Name))
	}
	if group.Status == nil || group.Status.ObservedGeneration == nil || *group.Status.ObservedGeneration != group.Metadata.Generation {
		return r.updateFailure(ctx, publication, "Pending", "InventoryNotObserved", fmt.Sprintf("InventoryCaptureGroup %q has not observed generation %d", group.Metadata.Name, group.Metadata.Generation))
	}
	if group.Status.Inventory == nil {
		return r.updateFailure(ctx, publication, "Pending", "InventoryNotReady", fmt.Sprintf("InventoryCaptureGroup %q has no rendered inventory", group.Metadata.Name))
	}

	artifacts, err := renderAnsibleDirectory(*group.Status.Inventory, publication.Spec.Target)
	if err != nil {
		return r.updateFailure(ctx, publication, "Failed", "RenderFailed", err.Error())
	}
	digest := artifactSetDigest(artifacts)

	destination := publication.Spec.DestinationRef
	if destination.ApiVersion != registry.GitRepositoryResource.APIVersion || destination.Kind != registry.GitRepositoryResource.Kind {
		return r.updateFailure(ctx, publication, "Failed", "UnsupportedDestination", fmt.Sprintf("destination %s %s is not supported", destination.ApiVersion, destination.Kind))
	}
	conflict, err := r.conflictingPublication(ctx, publication)
	if err != nil {
		return err
	}
	if conflict != "" {
		return r.updateFailure(ctx, publication, "Failed", "TargetOwnershipConflict", fmt.Sprintf("InventoryPublication %q owns an overlapping destination path", conflict))
	}
	repositoryRaw, err := r.store.Get(ctx, registry.GitRepositoryResource.Kind, destination.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.updateFailure(ctx, publication, "Pending", "DestinationNotFound", fmt.Sprintf("GitRepository %q does not exist", destination.Name))
	}
	if err != nil {
		return fmt.Errorf("get GitRepository %q: %w", destination.Name, err)
	}
	repository, err := registry.GitRepositoryResource.Decode(repositoryRaw)
	if err != nil {
		return fmt.Errorf("decode GitRepository %q: %w", destination.Name, err)
	}
	if publicationUpToDate(publication, group, repository, digest) {
		return nil
	}

	result, err := r.publisher.Publish(ctx, PublishRequest{
		Repository:      repository.Spec,
		PublicationName: publication.Metadata.Name,
		Branch:          publication.Spec.Target.Branch,
		RootPath:        publication.Spec.Target.RootPath,
		Artifacts:       artifacts,
	})
	if err != nil {
		return r.updateFailure(ctx, publication, "Failed", "PublicationFailed", err.Error())
	}
	return r.updateSuccess(ctx, publication, group, repository, digest, artifacts, result)
}

func (r *Reconciler) conflictingPublication(ctx context.Context, publication registry.InventoryPublication) (string, error) {
	root, err := safePublicationRoot(publication.Spec.Target.RootPath)
	if err != nil {
		return "", err
	}
	publications, err := r.store.List(ctx, registry.InventoryPublicationResource.Kind)
	if err != nil {
		return "", fmt.Errorf("list InventoryPublications for target ownership: %w", err)
	}
	for _, raw := range publications.Items {
		if raw.Metadata.UID == publication.Metadata.UID || raw.Metadata.Name == publication.Metadata.Name {
			continue
		}
		other, err := registry.InventoryPublicationResource.Decode(raw)
		if err != nil {
			return "", fmt.Errorf("decode InventoryPublication %q for target ownership: %w", raw.Metadata.Name, err)
		}
		if other.Spec.DestinationRef != publication.Spec.DestinationRef {
			continue
		}
		if other.Spec.Target.Branch != publication.Spec.Target.Branch {
			continue
		}
		otherRoot, err := safePublicationRoot(other.Spec.Target.RootPath)
		if err != nil {
			return "", fmt.Errorf("validate InventoryPublication %q target: %w", other.Metadata.Name, err)
		}
		if publicationRootsOverlap(root, otherRoot) {
			return other.Metadata.Name, nil
		}
	}
	return "", nil
}

func publicationRootsOverlap(left, right string) bool {
	return left == "." || right == "." || left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func (r *Reconciler) RequestsForCaptureGroup(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(publication registry.InventoryPublication) bool {
		return publication.Spec.InventoryCaptureGroupRef.Name == request.Name
	})
}

func (r *Reconciler) RequestsForGitRepository(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	return r.requestsMatching(ctx, func(publication registry.InventoryPublication) bool {
		return publication.Spec.DestinationRef.ApiVersion == registry.GitRepositoryResource.APIVersion &&
			publication.Spec.DestinationRef.Kind == registry.GitRepositoryResource.Kind &&
			publication.Spec.DestinationRef.Name == request.Name
	})
}

func (r *Reconciler) requestsMatching(ctx context.Context, matches func(registry.InventoryPublication) bool) ([]controller.Request, error) {
	publications, err := r.store.List(ctx, registry.InventoryPublicationResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list InventoryPublications: %w", err)
	}
	requests := make([]controller.Request, 0, len(publications.Items))
	for _, raw := range publications.Items {
		publication, err := registry.InventoryPublicationResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode InventoryPublication %q: %w", raw.Metadata.Name, err)
		}
		if matches(publication) {
			requests = append(requests, controller.Request{Kind: publication.Kind, Name: publication.Metadata.Name})
		}
	}
	return requests, nil
}

func renderAnsibleDirectory(inventory map[string]apigen.InventoryCaptureAnsibleGroup, target apigen.InventoryPublicationTarget) ([]Artifact, error) {
	if string(target.Layout) != "ansible-directory" {
		return nil, fmt.Errorf("unsupported publication layout %q", target.Layout)
	}
	renderedInventory := make(map[string]any, len(inventory))
	artifacts := make([]Artifact, 0, len(inventory)+1)
	for groupName, group := range inventory {
		renderedHosts, err := jsonObject(group.Hosts)
		if err != nil {
			return nil, fmt.Errorf("render inventory group %q hosts: %w", groupName, err)
		}
		renderedGroup := map[string]any{"hosts": renderedHosts}
		renderedInventory[groupName] = renderedGroup
		if group.Vars != nil {
			content, err := yaml.Marshal(*group.Vars)
			if err != nil {
				return nil, fmt.Errorf("render group_vars/%s.yaml: %w", groupName, err)
			}
			artifacts = append(artifacts, Artifact{Path: path.Join("group_vars", groupName+".yaml"), Content: content})
		}
	}
	inventoryContent, err := yaml.Marshal(renderedInventory)
	if err != nil {
		return nil, fmt.Errorf("render Ansible inventory: %w", err)
	}
	artifacts = append(artifacts, Artifact{Path: target.InventoryFile, Content: inventoryContent})
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].Path < artifacts[right].Path })
	return artifacts, nil
}

func jsonObject(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func artifactSetDigest(artifacts []Artifact) string {
	hash := sha256.New()
	for _, artifact := range artifacts {
		_, _ = hash.Write([]byte(artifact.Path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(artifact.Content)
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func publicationRequiresReady(spec apigen.InventoryPublicationSpec) bool {
	return spec.Policy == nil || spec.Policy.RequireReady == nil || *spec.Policy.RequireReady
}

func publicationUpToDate(publication registry.InventoryPublication, group registry.InventoryCaptureGroup, repository registry.GitRepository, digest string) bool {
	status := publication.Status
	if status == nil || status.Phase == nil || *status.Phase != "Published" || status.ObservedArtifactDigest == nil || *status.ObservedArtifactDigest != digest {
		return false
	}
	if status.ObservedGeneration == nil || *status.ObservedGeneration != publication.Metadata.Generation {
		return false
	}
	return status.Source != nil && status.Destination != nil &&
		status.Source.InventoryCaptureGroupUID == group.Metadata.UID &&
		status.Source.ObservedGeneration == group.Metadata.Generation &&
		status.Source.Digest == digest &&
		status.Destination.RepositoryUID == repository.Metadata.UID &&
		status.Destination.ObservedRepositoryGeneration == repository.Metadata.Generation &&
		status.Destination.Branch == publication.Spec.Target.Branch
}

func (r *Reconciler) updateSuccess(ctx context.Context, publication registry.InventoryPublication, group registry.InventoryCaptureGroup, repository registry.GitRepository, digest string, artifacts []Artifact, result PublishResult) error {
	now := r.now().UTC()
	publishedArtifacts := make([]apigen.InventoryPublicationArtifactStatus, 0, len(artifacts))
	for _, artifact := range artifacts {
		publishedArtifacts = append(publishedArtifacts, apigen.InventoryPublicationArtifactStatus{
			Path: path.Join(publication.Spec.Target.RootPath, artifact.Path), Digest: contentDigest(artifact.Content),
		})
	}
	phase, message, observedGeneration := "Published", "Inventory is published", publication.Metadata.Generation
	conditions := []apigen.InventoryPublicationCondition{{
		Type: "Ready", Status: apigen.InventoryPublicationConditionStatusTrue, Reason: "PublicationSucceeded",
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status := &apigen.InventoryPublicationStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration, ObservedArtifactDigest: &digest,
		LastPublishedTime: &now, Artifacts: &publishedArtifacts,
		Source: &apigen.InventoryPublicationSourceStatus{
			InventoryCaptureGroupUID: group.Metadata.UID, ObservedGeneration: group.Metadata.Generation, Digest: digest,
		},
		Destination: &apigen.InventoryPublicationDestinationStatus{
			RepositoryUID: repository.Metadata.UID, ObservedRepositoryGeneration: repository.Metadata.Generation,
			Branch: publication.Spec.Target.Branch, Revision: result.Revision, Url: &result.URL,
		},
		Conditions: &conditions,
	}
	return r.writeStatus(ctx, publication, status)
}

func (r *Reconciler) updateFailure(ctx context.Context, publication registry.InventoryPublication, phase, reason, message string) error {
	status := &apigen.InventoryPublicationStatus{}
	if publication.Status != nil {
		*status = *publication.Status
	}
	observedGeneration := publication.Metadata.Generation
	conditions := []apigen.InventoryPublicationCondition{{
		Type: "Ready", Status: apigen.InventoryPublicationConditionStatusFalse, Reason: reason,
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status.Phase = &phase
	status.ObservedGeneration = &observedGeneration
	status.Conditions = &conditions
	return r.writeStatus(ctx, publication, status)
}

func (r *Reconciler) writeStatus(ctx context.Context, publication registry.InventoryPublication, status *apigen.InventoryPublicationStatus) error {
	if resource.EqualJSON(publication.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(publication.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse InventoryPublication %q resource version: %w", publication.Metadata.Name, err)
	}
	storedStatus, err := registry.InventoryPublicationResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	if _, err := r.store.UpdateStatus(ctx, publication.Kind, publication.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update InventoryPublication %q status: %w", publication.Metadata.Name, err)
	}
	return nil
}
