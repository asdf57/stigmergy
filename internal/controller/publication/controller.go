package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type Reconciler struct {
	store     store.Store
	publisher Publisher
	now       func() time.Time
}

func NewReconciler(store store.Store) *Reconciler {
	return &Reconciler{store: store, publisher: NewGitPublisher(), now: time.Now}
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
	if publicationRequiresReady(publication.Spec) && group.Status["phase"] != "Ready" {
		return r.updateFailure(ctx, publication, "Pending", "InventoryNotReady", fmt.Sprintf("InventoryCaptureGroup %q phase is %v", group.Metadata.Name, group.Status["phase"]))
	}
	if numberAsInt64(group.Status["observedGeneration"]) != group.Metadata.Generation {
		return r.updateFailure(ctx, publication, "Pending", "InventoryNotObserved", fmt.Sprintf("InventoryCaptureGroup %q has not observed generation %d", group.Metadata.Name, group.Metadata.Generation))
	}

	artifacts, err := renderAnsibleDirectory(group.Status["inventory"], publication.Spec.Target)
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
		RootPath:        publication.Spec.Target.RootPath,
		Artifacts:       artifacts,
	})
	if err != nil {
		return r.updateFailure(ctx, publication, "Failed", "PublicationFailed", err.Error())
	}
	return r.updateSuccess(ctx, publication, group, repository, digest, artifacts, result)
}

func (r *Reconciler) conflictingPublication(ctx context.Context, publication registry.InventoryPublication) (string, error) {
	root, err := safeRepositoryPath(publication.Spec.Target.RootPath)
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
		otherRoot, err := safeRepositoryPath(other.Spec.Target.RootPath)
		if err != nil {
			return "", fmt.Errorf("validate InventoryPublication %q target: %w", other.Metadata.Name, err)
		}
		if root == otherRoot || strings.HasPrefix(root, otherRoot+"/") || strings.HasPrefix(otherRoot, root+"/") {
			return other.Metadata.Name, nil
		}
	}
	return "", nil
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

func renderAnsibleDirectory(value any, target apigen.InventoryPublicationTarget) ([]Artifact, error) {
	if string(target.Layout) != "ansible-directory" {
		return nil, fmt.Errorf("unsupported publication layout %q", target.Layout)
	}
	inventory, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("InventoryCaptureGroup status does not contain inventory")
	}
	renderedInventory := make(map[string]any, len(inventory))
	artifacts := make([]Artifact, 0, len(inventory)+1)
	for groupName, rawGroup := range inventory {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("inventory group %q is not an object", groupName)
		}
		renderedGroup := make(map[string]any, len(group))
		for key, groupValue := range group {
			if key != "vars" {
				renderedGroup[key] = groupValue
			}
		}
		renderedInventory[groupName] = renderedGroup
		if variables, exists := group["vars"]; exists {
			content, err := yaml.Marshal(variables)
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
	if publication.Status["phase"] != "Published" || publication.Status["observedArtifactDigest"] != digest {
		return false
	}
	if numberAsInt64(publication.Status["observedGeneration"]) != publication.Metadata.Generation {
		return false
	}
	source, sourceOK := publication.Status["source"].(map[string]any)
	destination, destinationOK := publication.Status["destination"].(map[string]any)
	return sourceOK && destinationOK &&
		source["inventoryCaptureGroupUID"] == group.Metadata.UID &&
		numberAsInt64(source["observedGeneration"]) == group.Metadata.Generation &&
		source["digest"] == digest &&
		destination["repositoryUID"] == repository.Metadata.UID &&
		numberAsInt64(destination["observedRepositoryGeneration"]) == repository.Metadata.Generation
}

func numberAsInt64(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case float64:
		return int64(number)
	default:
		return -1
	}
}

func (r *Reconciler) updateSuccess(ctx context.Context, publication registry.InventoryPublication, group registry.InventoryCaptureGroup, repository registry.GitRepository, digest string, artifacts []Artifact, result PublishResult) error {
	now := r.now().UTC().Format(time.RFC3339Nano)
	publishedArtifacts := make([]any, 0, len(artifacts))
	for _, artifact := range artifacts {
		publishedArtifacts = append(publishedArtifacts, map[string]any{
			"path":   path.Join(publication.Spec.Target.RootPath, artifact.Path),
			"digest": contentDigest(artifact.Content),
		})
	}
	status := map[string]any{
		"phase":                  "Published",
		"observedGeneration":     publication.Metadata.Generation,
		"observedArtifactDigest": digest,
		"lastPublishedTime":      now,
		"artifacts":              publishedArtifacts,
		"source": map[string]any{
			"inventoryCaptureGroupUID": group.Metadata.UID,
			"observedGeneration":       group.Metadata.Generation,
			"digest":                   digest,
		},
		"destination": map[string]any{
			"repositoryUID":                repository.Metadata.UID,
			"observedRepositoryGeneration": repository.Metadata.Generation,
			"revision":                     result.Revision,
			"url":                          result.URL,
		},
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "PublicationSucceeded",
			"message": "Inventory is published", "observedGeneration": publication.Metadata.Generation,
		}},
	}
	return r.writeStatus(ctx, publication, status)
}

func (r *Reconciler) updateFailure(ctx context.Context, publication registry.InventoryPublication, phase, reason, message string) error {
	status := cloneStatus(publication.Status)
	delete(status, "observedInventoryDigest")
	status["phase"] = phase
	status["observedGeneration"] = publication.Metadata.Generation
	status["conditions"] = []any{map[string]any{
		"type": "Ready", "status": "False", "reason": reason,
		"message": message, "observedGeneration": publication.Metadata.Generation,
	}}
	return r.writeStatus(ctx, publication, status)
}

func (r *Reconciler) writeStatus(ctx context.Context, publication registry.InventoryPublication, status map[string]any) error {
	if resource.EqualJSON(publication.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(publication.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse InventoryPublication %q resource version: %w", publication.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, publication.Kind, publication.Metadata.Name, status, revision); err != nil {
		return fmt.Errorf("update InventoryPublication %q status: %w", publication.Metadata.Name, err)
	}
	return nil
}

func cloneStatus(status map[string]any) map[string]any {
	cloned := make(map[string]any, len(status)+2)
	for key, value := range status {
		cloned[key] = value
	}
	return cloned
}
