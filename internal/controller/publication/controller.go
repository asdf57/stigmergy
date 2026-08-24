package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

	content, err := renderInventory(group.Status["inventory"], publication.Spec.Format)
	if err != nil {
		return r.updateFailure(ctx, publication, "Failed", "RenderFailed", err.Error())
	}
	digest := inventoryDigest(content)

	destination := publication.Spec.DestinationRef
	if destination.ApiVersion != registry.GitRepositoryResource.APIVersion || destination.Kind != registry.GitRepositoryResource.Kind {
		return r.updateFailure(ctx, publication, "Failed", "UnsupportedDestination", fmt.Sprintf("destination %s %s is not supported", destination.ApiVersion, destination.Kind))
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
	if publicationUpToDate(publication, repository, digest) {
		return nil
	}

	result, err := r.publisher.Publish(ctx, PublishRequest{
		Repository:      repository.Spec,
		PublicationName: publication.Metadata.Name,
		Path:            publication.Spec.Path,
		Content:         content,
	})
	if err != nil {
		return r.updateFailure(ctx, publication, "Failed", "PublicationFailed", err.Error())
	}
	return r.updateSuccess(ctx, publication, repository, digest, result)
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

func renderInventory(value any, format apigen.InventoryPublicationSpecFormat) ([]byte, error) {
	if value == nil {
		return nil, errors.New("InventoryCaptureGroup status does not contain inventory")
	}
	switch string(format) {
	case "ansible-yaml":
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("render Ansible YAML: %w", err)
		}
		return encoded, nil
	case "ansible-json":
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("render Ansible JSON: %w", err)
		}
		return append(encoded, '\n'), nil
	default:
		return nil, fmt.Errorf("unsupported inventory format %q", format)
	}
}

func inventoryDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func publicationRequiresReady(spec apigen.InventoryPublicationSpec) bool {
	return spec.Policy == nil || spec.Policy.RequireReady == nil || *spec.Policy.RequireReady
}

func publicationUpToDate(publication registry.InventoryPublication, repository registry.GitRepository, digest string) bool {
	if publication.Status["phase"] != "Published" || publication.Status["observedInventoryDigest"] != digest {
		return false
	}
	if numberAsInt64(publication.Status["observedGeneration"]) != publication.Metadata.Generation {
		return false
	}
	destination, ok := publication.Status["destination"].(map[string]any)
	return ok && destination["repositoryUID"] == repository.Metadata.UID &&
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

func (r *Reconciler) updateSuccess(ctx context.Context, publication registry.InventoryPublication, repository registry.GitRepository, digest string, result PublishResult) error {
	now := r.now().UTC().Format(time.RFC3339Nano)
	status := map[string]any{
		"phase":                   "Published",
		"observedGeneration":      publication.Metadata.Generation,
		"observedInventoryDigest": digest,
		"lastPublishedTime":       now,
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
