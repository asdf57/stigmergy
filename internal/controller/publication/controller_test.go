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
	if !strings.Contains(string(publisher.requests[0].Content), "ansible_host: 10.1.1.251") {
		t.Fatalf("published content = %s", publisher.requests[0].Content)
	}
	status := storage.resources["InventoryPublication/servers-git"].Status
	if status["phase"] != "Published" || status["observedInventoryDigest"] == "" {
		t.Fatalf("publication status = %#v", status)
	}
	if status["destination"].(map[string]any)["revision"] != "abc123" {
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
			"phase": phase,
			"inventory": map[string]any{"servers": map[string]any{"hosts": map[string]any{
				"desktop": map[string]any{"ansible_host": "10.1.1.251"},
			}}},
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
			"format":                   "ansible-yaml",
			"destinationRef": map[string]any{
				"apiVersion": registry.GitRepositoryResource.APIVersion,
				"kind":       registry.GitRepositoryResource.Kind,
				"name":       "infrastructure",
			},
			"path": "ansible/inventory/homelab.yaml",
		},
		Status: map[string]any{},
	}
}
