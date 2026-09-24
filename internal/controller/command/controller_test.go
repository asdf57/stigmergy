package command

import (
	"context"
	"strconv"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/controller/publication"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

type fakeStore struct {
	resources map[string]resource.Resource
}

func newFakeStore(values ...resource.Resource) *fakeStore {
	result := &fakeStore{resources: make(map[string]resource.Resource)}
	for _, value := range values {
		result.resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return result
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
	return value, nil
}
func (f *fakeStore) Delete(context.Context, string, string, int64) error { panic("unexpected Delete") }
func (f *fakeStore) DeleteCollection(context.Context, string) (int64, error) {
	panic("unexpected DeleteCollection")
}
func (f *fakeStore) Ready(context.Context) error { return nil }

type fakePublisher struct {
	requests []publication.PublishRequest
	result   publication.PublishResult
}

func (f *fakePublisher) Publish(_ context.Context, request publication.PublishRequest) (publication.PublishResult, error) {
	f.requests = append(f.requests, request)
	return f.result, nil
}

func TestReconcilePublishesScriptToCaptureGroupBranch(t *testing.T) {
	command := testCommand("servers", "#!/usr/bin/env bash\nansible all -m ping\n")
	pipeline := testPipeline()
	repository := testRepository()
	storage := newFakeStore(command, pipeline, repository)
	publisher := &fakePublisher{result: publication.PublishResult{Revision: "abc123", Changed: true}}

	if err := NewReconcilerWithPublisher(storage, publisher).Reconcile(context.Background(), controller.Request{
		Kind: command.Kind, Name: command.Metadata.Name,
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 1 {
		t.Fatalf("Publish() calls = %d, want 1", len(publisher.requests))
	}
	request := publisher.requests[0]
	if request.Branch != "servers" || request.RootPath != "." || !request.PreserveUnmanaged {
		t.Fatalf("publish request = %#v", request)
	}
	if len(request.Artifacts) != 1 || request.Artifacts[0].Path != "servers.sh" || string(request.Artifacts[0].Content) != command.Spec["script"] {
		t.Fatalf("published artifacts = %#v", request.Artifacts)
	}
	status := storage.resources[registry.CommandResource.Kind+"/servers-command"].Status
	if status["phase"] != "Published" || status["revision"] != "abc123" {
		t.Fatalf("Command status = %#v", status)
	}
}

func TestReconcileRejectsMultipleCommandsForOneCaptureGroup(t *testing.T) {
	first := testCommand("servers", "echo first\n")
	second := testCommand("servers", "echo second\n")
	second.Metadata.Name = "other-command"
	second.Metadata.UID = "other-command-uid"
	storage := newFakeStore(first, second, testPipeline(), testRepository())
	publisher := &fakePublisher{}

	if err := NewReconcilerWithPublisher(storage, publisher).Reconcile(context.Background(), controller.Request{
		Kind: first.Kind, Name: first.Metadata.Name,
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(publisher.requests) != 0 {
		t.Fatalf("Publish() calls = %d, want 0", len(publisher.requests))
	}
	status := storage.resources[registry.CommandResource.Kind+"/servers-command"].Status
	if status["phase"] != "Conflict" {
		t.Fatalf("Command status = %#v", status)
	}
}

func TestReconcileWaitsForCommandsPipeline(t *testing.T) {
	value := testCommand("servers", "echo waiting\n")
	storage := newFakeStore(value)
	publisher := &fakePublisher{}

	if err := NewReconcilerWithPublisher(storage, publisher).Reconcile(context.Background(), controller.Request{
		Kind: value.Kind, Name: value.Metadata.Name,
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	status := storage.resources[registry.CommandResource.Kind+"/servers-command"].Status
	if status["phase"] != "Pending" {
		t.Fatalf("Command status = %#v", status)
	}
}

func testCommand(group, script string) resource.Resource {
	return resource.Resource{
		APIVersion: registry.CommandResource.APIVersion,
		Kind:       registry.CommandResource.Kind,
		Metadata:   resource.Metadata{Name: "servers-command", UID: "command-uid", ResourceVersion: "1", Generation: 1},
		Spec: map[string]any{
			"inventoryCaptureGroupRef": map[string]any{"name": group},
			"script":                   script,
		},
	}
}

func testPipeline() resource.Resource {
	return resource.Resource{
		APIVersion: registry.CommandsPipelineResource.APIVersion,
		Kind:       registry.CommandsPipelineResource.Kind,
		Metadata:   resource.Metadata{Name: "servers", UID: "pipeline-uid", ResourceVersion: "1", Generation: 2},
		Spec: map[string]any{
			"commandsRepositoryRef":    map[string]any{"name": "commands-data"},
			"inventoryCaptureGroupRef": map[string]any{"name": "servers"},
			"pipelineProviderRef":      map[string]any{"name": "concourse"},
			"commandPath":              "servers.sh",
		},
		Status: map[string]any{"phase": string(apigen.CommandsPipelineStatusPhaseReady), "observedGeneration": int64(2)},
	}
}

func testRepository() resource.Resource {
	return resource.Resource{
		APIVersion: registry.GitRepositoryResource.APIVersion,
		Kind:       registry.GitRepositoryResource.Kind,
		Metadata:   resource.Metadata{Name: "commands-data", UID: "repository-uid", ResourceVersion: "1", Generation: 1},
		Spec:       map[string]any{"url": "https://example.test/commands.git"},
	}
}
