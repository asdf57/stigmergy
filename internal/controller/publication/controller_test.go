package publication

import (
	"context"
	"strconv"
	"strings"
	"testing"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type fakeStore struct {
	resources     map[string]resource.Resource
	statusUpdates []resource.Resource
}

func newFakeStore(resources ...resource.Resource) *fakeStore {
	f := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range resources {
		f.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return f
}

func (f *fakeStore) Create(context.Context, resource.Resource) (resource.Resource, error) {
	panic("unexpected Create")
}
func (f *fakeStore) Get(_ context.Context, kind, name string) (resource.Resource, error) {
	value, found := f.resources[kind+"/"+name]
	if !found {
		return resource.Resource{}, store.ErrNotFound
	}
	return value, nil
}
func (f *fakeStore) List(_ context.Context, kind string) (resource.List, error) {
	result := resource.List{}
	for _, value := range f.resources {
		if value.Kind == kind {
			result.Items = append(result.Items, value)
		}
	}
	return result, nil
}
func (f *fakeStore) Update(context.Context, resource.Resource, int64) (resource.Resource, error) {
	panic("unexpected Update")
}
func (f *fakeStore) UpdateStatus(_ context.Context, kind, name string, status map[string]any, revision int64) (resource.Resource, error) {
	key := kind + "/" + name
	value, found := f.resources[key]
	if !found {
		return resource.Resource{}, store.ErrNotFound
	}
	if value.Metadata.ResourceVersion != strconv.FormatInt(revision, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	value.Status = status
	value.Metadata.ResourceVersion = strconv.FormatInt(revision+1, 10)
	f.resources[key] = value
	f.statusUpdates = append(f.statusUpdates, value)
	return value, nil
}
func (f *fakeStore) Delete(context.Context, string, string, int64) error {
	panic("unexpected Delete")
}
func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) {
	panic("unexpected DeleteCollection")
}
func (f *fakeStore) Ready(context.Context) error { return nil }

type fakePublisher struct {
	requests []PublishRequest
	result   PublishResult
	err      error
}

func (p *fakePublisher) Publish(_ context.Context, request PublishRequest) (PublishResult, error) {
	p.requests = append(p.requests, request)
	return p.result, p.err
}

func TestReconcilePublishesReadyInventoryOncePerDigest(t *testing.T) {
	group := publicationTestGroup("Ready")
	repository := publicationTestRepository()
	publication := publicationTestResource()
	storage := newFakeStore(group, repository, publication)
	publisher := &fakePublisher{result: PublishResult{Revision: "abc123", URL: "https://example.test/repo.git", Changed: true}}
	reconciler := NewReconcilerWithPublisher(storage, publisher)

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: publication.Kind, Name: publication.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 1 {
		t.Fatalf("Publish() calls = %d, want 1", len(publisher.requests))
	}
	if publisher.requests[0].Branch != "servers-inventory" || publisher.requests[0].RootPath != "inventories/servers" || len(publisher.requests[0].Artifacts) != 3 {
		t.Fatalf("publish request = %#v", publisher.requests[0])
	}
	artifactContent := make(map[string]string)
	for _, artifact := range publisher.requests[0].Artifacts {
		artifactContent[artifact.Path] = string(artifact.Content)
	}
	if !strings.Contains(artifactContent["inventory.yaml"], "ansible_host: 10.1.1.251") || strings.Contains(artifactContent["inventory.yaml"], "ansible_user") {
		t.Fatalf("inventory artifact = %s", artifactContent["inventory.yaml"])
	}
	if !strings.Contains(artifactContent["group_vars/all.yaml"], "ansible_user: matt") || !strings.Contains(artifactContent["group_vars/workstations.yaml"], "desktop_environment: true") {
		t.Fatalf("group variable artifacts = %#v", artifactContent)
	}
	status := storage.resources["InventoryPublication/servers-git"].Status
	if status["phase"] != "Published" || status["observedArtifactDigest"] == "" {
		t.Fatalf("publication status = %#v", status)
	}
	if status["destination"].(map[string]any)["revision"] != "abc123" {
		t.Fatalf("destination status = %#v", status["destination"])
	}
	if status["destination"].(map[string]any)["branch"] != "servers-inventory" {
		t.Fatalf("destination status = %#v", status["destination"])
	}

	if err := reconciler.Reconcile(context.Background(), controller.Request{Kind: publication.Kind, Name: publication.Metadata.Name}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 1 {
		t.Fatalf("unchanged inventory caused %d Publish() calls, want 1", len(publisher.requests))
	}
}

func TestReconcileWaitsForReadyInventoryByDefault(t *testing.T) {
	group := publicationTestGroup("Partial")
	repository := publicationTestRepository()
	publication := publicationTestResource()
	storage := newFakeStore(group, repository, publication)
	publisher := &fakePublisher{}

	if err := NewReconcilerWithPublisher(storage, publisher).Reconcile(context.Background(), controller.Request{Kind: publication.Kind, Name: publication.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 0 {
		t.Fatalf("Publish() calls = %d, want 0", len(publisher.requests))
	}
	status := storage.resources["InventoryPublication/servers-git"].Status
	if status["phase"] != "Pending" {
		t.Fatalf("phase = %#v, want Pending", status["phase"])
	}
}

func TestReconcileRejectsOverlappingPublicationRoots(t *testing.T) {
	group := publicationTestGroup("Ready")
	repository := publicationTestRepository()
	publication := publicationTestResource()
	other := publicationTestResource()
	other.Metadata.Name = "all-inventories"
	other.Metadata.UID = "other-publication-uid"
	other.Spec["target"].(map[string]any)["rootPath"] = "inventories"
	storage := newFakeStore(group, repository, publication, other)
	publisher := &fakePublisher{}

	if err := NewReconcilerWithPublisher(storage, publisher).Reconcile(context.Background(), controller.Request{Kind: publication.Kind, Name: publication.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 0 {
		t.Fatalf("Publish() calls = %d, want 0", len(publisher.requests))
	}
	status := storage.resources["InventoryPublication/servers-git"].Status
	condition := status["conditions"].([]any)[0].(map[string]any)
	if status["phase"] != "Failed" || condition["reason"] != "TargetOwnershipConflict" {
		t.Fatalf("publication status = %#v", status)
	}
}

func TestReconcileAllowsOverlappingPublicationRootsOnDifferentBranches(t *testing.T) {
	group := publicationTestGroup("Ready")
	repository := publicationTestRepository()
	publication := publicationTestResource()
	other := publicationTestResource()
	other.Metadata.Name = "other-branch"
	other.Metadata.UID = "other-publication-uid"
	other.Spec["target"].(map[string]any)["branch"] = "other-inventory"
	storage := newFakeStore(group, repository, publication, other)
	publisher := &fakePublisher{result: PublishResult{Revision: "abc123", URL: "https://example.test/repo.git", Changed: true}}

	if err := NewReconcilerWithPublisher(storage, publisher).Reconcile(context.Background(), controller.Request{Kind: publication.Kind, Name: publication.Metadata.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 1 {
		t.Fatalf("Publish() calls = %d, want 1", len(publisher.requests))
	}
}

func TestCredentialEnvironmentVariableIsRequiredWhenConfigured(t *testing.T) {
	publisher := &GitPublisher{LookupEnv: func(string) (string, bool) { return "", false }}
	_, err := publisher.authentication(apigen.GitRepositorySpec{
		Authentication: &apigen.GitRepositoryAuthentication{PasswordEnvironmentVariable: "GITHUB_TOKEN"},
	})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("authentication() error = %v", err)
	}
}

func publicationTestGroup(phase string) resource.Resource {
	return resource.Resource{
		APIVersion: registry.InventoryCaptureGroupResource.APIVersion,
		Kind:       registry.InventoryCaptureGroupResource.Kind,
		Metadata:   resource.Metadata{Name: "servers", UID: "group-uid", ResourceVersion: "1", Generation: 1},
		Spec:       map[string]any{"selector": map[string]any{}},
		Status: map[string]any{
			"phase":              phase,
			"observedGeneration": int64(1),
			"inventory": map[string]any{
				"all": map[string]any{
					"hosts": map[string]any{"desktop": map[string]any{"ansible_host": "10.1.1.251"}},
					"vars":  map[string]any{"ansible_user": "matt"},
				},
				"workstations": map[string]any{
					"hosts": map[string]any{"desktop": map[string]any{"ansible_host": "10.1.1.251"}},
					"vars":  map[string]any{"desktop_environment": true},
				},
			},
		},
	}
}

func publicationTestRepository() resource.Resource {
	return resource.Resource{
		APIVersion: registry.GitRepositoryResource.APIVersion,
		Kind:       registry.GitRepositoryResource.Kind,
		Metadata:   resource.Metadata{Name: "infrastructure", UID: "repository-uid", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{
			"url": "https://example.test/repo.git", "branch": "main",
		},
	}
}

func publicationTestResource() resource.Resource {
	return resource.Resource{
		APIVersion: registry.InventoryPublicationResource.APIVersion,
		Kind:       registry.InventoryPublicationResource.Kind,
		Metadata:   resource.Metadata{Name: "servers-git", UID: "publication-uid", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{
			"inventoryCaptureGroupRef": map[string]any{"name": "servers"},
			"destinationRef": map[string]any{
				"apiVersion": registry.GitRepositoryResource.APIVersion,
				"kind":       registry.GitRepositoryResource.Kind,
				"name":       "infrastructure",
			},
			"target": map[string]any{
				"branch": "servers-inventory", "rootPath": "inventories/servers", "layout": "ansible-directory", "inventoryFile": "inventory.yaml",
			},
		},
		Status: map[string]any{},
	}
}
