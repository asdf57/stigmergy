package command

import (
	"context"
	"errors"
	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/controller/commandspipeline"
	"github.com/asdf57/stigmergy/internal/controller/pipeline"
	"github.com/asdf57/stigmergy/internal/gitpublication"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakePublisher struct {
	requests []gitpublication.PublishRequest
}

func (f *fakePublisher) Publish(_ context.Context, q gitpublication.PublishRequest) (gitpublication.PublishResult, error) {
	f.requests = append(f.requests, q)
	return gitpublication.PublishResult{Revision: "abc123"}, nil
}

type fakeBackend struct {
	triggers, aborts int
	builds           []pipeline.Build
	err              error
	onTrigger        func()
}

func (b *fakeBackend) CheckResource(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string, string, map[string]string) error {
	return nil
}

func (b *fakeBackend) Trigger(_ context.Context, _ registry.PipelineProvider, _ registry.UsernamePasswordCredential, _ string) (pipeline.Build, error) {
	b.triggers++
	if b.onTrigger != nil {
		b.onTrigger()
	}
	if b.err != nil {
		return pipeline.Build{}, b.err
	}
	build := pipeline.Build{ID: int64(41 + b.triggers), Status: "pending"}
	b.builds = append(b.builds, build)
	return build, nil
}
func (b *fakeBackend) Builds(context.Context, registry.PipelineProvider, registry.UsernamePasswordCredential, string) ([]pipeline.Build, error) {
	return b.builds, nil
}
func (b *fakeBackend) Build(_ context.Context, _ registry.PipelineProvider, _ registry.UsernamePasswordCredential, _ string, id int64) (pipeline.Build, error) {
	for _, v := range b.builds {
		if v.ID == id {
			return v, nil
		}
	}
	return pipeline.Build{}, errors.New("build missing")
}
func (b *fakeBackend) Abort(_ context.Context, _ registry.PipelineProvider, _ registry.UsernamePasswordCredential, id int64) error {
	b.aborts++
	for i := range b.builds {
		if b.builds[i].ID == id {
			b.builds[i].Status = "aborted"
		}
	}
	return nil
}

func fixture(t *testing.T) (*Reconciler, *testutil.Store, *fakePublisher, *fakeBackend) {
	t.Helper()
	resources := []resource.Resource{
		{APIVersion: resource.APIVersion, Kind: "CommandsPipeline", Metadata: resource.Metadata{Name: "servers", UID: "executor-uid", ResourceVersion: "1", Generation: 1}, Spec: map[string]any{"commandsRepositoryRef": map[string]any{"name": "repo"}, "inventoryCaptureGroupRef": map[string]any{"name": "servers"}, "pipelineProviderRef": map[string]any{"name": "concourse"}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1}},
		{APIVersion: resource.APIVersion, Kind: "GitRepository", Metadata: resource.Metadata{Name: "repo", UID: "repo-uid", ResourceVersion: "1", Generation: 1}, Spec: map[string]any{"url": "https://example/commands.git"}},
		{APIVersion: resource.APIVersion, Kind: "InventoryCaptureGroup", Metadata: resource.Metadata{Name: "servers", UID: "group-uid", ResourceVersion: "1", Generation: 1}, Spec: map[string]any{}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1}},
		{APIVersion: resource.APIVersion, Kind: "PipelineProvider", Metadata: resource.Metadata{Name: "concourse", UID: "provider-uid", ResourceVersion: "1", Generation: 1}, Spec: map[string]any{"type": "concourse", "url": "https://ci.example", "team": "main", "credentialRef": map[string]any{"name": "login"}}, Status: map[string]any{"phase": "Ready", "observedGeneration": 1}},
		{APIVersion: resource.APIVersion, Kind: "UsernamePasswordCredential", Metadata: resource.Metadata{Name: "login", UID: "credential-uid", ResourceVersion: "1", Generation: 1}, Spec: map[string]any{"username": "test", "password": "password"}},
	}
	s := testutil.NewStore(resources...)
	publisher := &fakePublisher{}
	backend := &fakeBackend{}
	r := NewReconcilerWithDependencies(s, commandspipeline.Config{CommandRunnerImage: "runner:v1", PublicAPIURL: "https://api.example", AnsibleRolesRepository: "https://example/roles", AnsibleRolesRevision: "main"}, publisher, backend)
	return r, s, publisher, backend
}
func addCommand(t *testing.T, s *testutil.Store, name string, ttl *int64) resource.Resource {
	t.Helper()
	v := registry.NewCommand(resource.Metadata{Name: name, Finalizers: append([]string(nil), registry.CommandResource.DefaultFinalizers...)}, apigen.CommandSpec{CommandsPipelineRef: apigen.CommandExecutorReference{Name: "servers"}, Script: "echo " + name, TtlSecondsAfterFinished: ttl})
	raw, err := v.Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = s.Create(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func reconcile(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	if err := r.Reconcile(context.Background(), controller.Request{Name: name}); err != nil {
		t.Fatal(err)
	}
}
func readyChild(t *testing.T, s *testutil.Store, uid string) {
	t.Helper()
	name := commandspipeline.ExecutionName("servers")
	raw, err := s.Get(context.Background(), "Pipeline", name)
	if err != nil {
		t.Fatal(err)
	}
	rv, _ := strconv.ParseInt(raw.Metadata.ResourceVersion, 10, 64)
	_, err = s.UpdateStatus(context.Background(), "Pipeline", name, map[string]any{"phase": "Ready", "observedGeneration": raw.Metadata.Generation}, rv)
	if err != nil {
		t.Fatal(err)
	}
}
func status(t *testing.T, s *testutil.Store, name string) registry.Command {
	t.Helper()
	raw, err := s.Get(context.Background(), "Command", name)
	if err != nil {
		t.Fatal(err)
	}
	v, err := registry.CommandResource.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func prepared(t *testing.T, r *Reconciler, s *testutil.Store, name string) resource.Resource {
	raw := addCommand(t, s, name, nil)
	reconcile(t, r, name)
	reconcile(t, r, name)
	reconcile(t, r, name)
	readyChild(t, s, raw.Metadata.UID)
	return raw
}
func TestCommandsSharePipelineWithoutInputRaces(t *testing.T) {
	r, s, p, b := fixture(t)
	first := prepared(t, r, s, "first")
	second := prepared(t, r, s, "second")
	if executionName(first.Metadata.UID) == executionName(second.Metadata.UID) {
		t.Fatal("execution collision")
	}
	if len(p.requests) != 2 || p.requests[0].RootPath == p.requests[1].RootPath || p.requests[0].OwnerUID != first.Metadata.UID {
		t.Fatal(p.requests)
	}
	child, _ := s.Get(context.Background(), "Pipeline", commandspipeline.ExecutionName("servers"))
	data := child.Spec["definition"].(map[string]any)["data"].(string)
	if !strings.Contains(data, "ref: abc123") || strings.Contains(data, "trigger: true") || strings.Contains(data, "type: time") {
		t.Fatal(data)
	}
	b.onTrigger = func() {
		if *status(t, s, "first").Status.Phase != apigen.CommandStatusPhaseDispatching {
			t.Fatal("dispatch was not persisted before submit")
		}
	}
	reconcile(t, r, "first")
	b.onTrigger = nil
	for i := 0; i < 3; i++ {
		reconcile(t, r, "first")
	}
	if b.triggers != 1 {
		t.Fatal("build resubmitted")
	}
	reconcile(t, r, "second")
	if b.triggers != 1 || (*status(t, s, "second").Status.Conditions)[0].Reason != "ExecutorBusy" {
		t.Fatal("second request altered an active job")
	}
	unchanged, _ := s.Get(context.Background(), "Pipeline", child.Metadata.Name)
	if !resource.EqualJSON(child.Spec, unchanged.Spec) {
		t.Fatal("active job input changed")
	}
	b.builds[0].Status = "succeeded"
	reconcile(t, r, "first")
	reconcile(t, r, "first") // release the durable executor slot
	writes := s.Writes
	for i := 0; i < 3; i++ {
		reconcile(t, r, "first")
	}
	if b.triggers != 1 || s.Writes != writes || *status(t, s, "first").Status.Phase != apigen.CommandStatusPhaseSucceeded {
		t.Fatal("terminal execution was replayed")
	}
	reconcile(t, r, "second")
	readyChild(t, s, second.Metadata.UID)
	reconcile(t, r, "second")
	if b.triggers != 2 || len(b.builds) != 2 {
		t.Fatal("separate build not submitted")
	}
	if status(t, s, "first").Status.PipelineRef.Uid != status(t, s, "second").Status.PipelineRef.Uid {
		t.Fatal("commands did not share one pipeline")
	}
	if p.requests[0].Branch != "commands-servers" || p.requests[1].Branch != p.requests[0].Branch {
		t.Fatal("commands did not share a stable branch")
	}
}
func TestLostSubmissionAdoptedWithoutRetrigger(t *testing.T) {
	r, s, _, b := fixture(t)
	prepared(t, r, s, "lost")
	b.err = errors.New("connection lost")
	reconcile(t, r, "lost")
	reconcile(t, r, "lost")
	if b.triggers != 1 || *status(t, s, "lost").Status.Phase != apigen.CommandStatusPhaseDispatching {
		t.Fatal("uncertain submission retried")
	}
	// Restart with the same persisted state; an eventually visible build is adopted.
	restarted := NewReconcilerWithDependencies(s, r.executorConfigForTest(), r.publisher, b)
	b.builds = []pipeline.Build{{ID: 123, Status: "started"}}
	reconcile(t, restarted, "lost")
	if b.triggers != 1 || *status(t, s, "lost").Status.BuildID != 123 {
		t.Fatal("build not safely adopted")
	}
}
func (r *Reconciler) executorConfigForTest() commandspipeline.Config {
	return commandspipeline.Config{CommandRunnerImage: "runner:v1", PublicAPIURL: "https://api.example", AnsibleRolesRepository: "https://example/roles", AnsibleRolesRevision: "main"}
}
func TestTTLAndOwnedCleanup(t *testing.T) {
	r, s, p, b := fixture(t)
	ttl := int64(5)
	raw := addCommand(t, s, "ttl", &ttl)
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		reconcile(t, r, "ttl")
	}
	readyChild(t, s, raw.Metadata.UID)
	reconcile(t, r, "ttl")
	b.builds[0].Status = "failed"
	reconcile(t, r, "ttl")
	reconcile(t, r, "ttl")
	if status(t, s, "ttl").Metadata.DeletionTimestamp != nil {
		t.Fatal("early TTL cleanup")
	}
	now = now.Add(5 * time.Second)
	reconcile(t, r, "ttl")
	reconcile(t, r, "ttl")
	childName := commandspipeline.ExecutionName("servers")
	child, _ := s.Get(context.Background(), "Pipeline", childName)
	if child.Metadata.DeletionTimestamp != nil {
		t.Fatal("shared pipeline was deleted")
	}
	if _, ok := s.Resources["Command/ttl"]; ok {
		t.Fatal("command not removed")
	}
	if !p.requests[len(p.requests)-1].RemoveOwned || p.requests[len(p.requests)-1].PreserveUnmanaged {
		t.Fatal("Git inputs not cleaned")
	}
}
func TestDeletingRunningCommandAbortsOnlyItsBuild(t *testing.T) {
	r, s, _, b := fixture(t)
	raw := prepared(t, r, s, "cancel")
	reconcile(t, r, "cancel")
	current, _ := s.Get(context.Background(), "Command", "cancel")
	rv, _ := version(current.Metadata)
	if err := s.Delete(context.Background(), "Command", "cancel", rv); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "cancel")
	_ = raw
	child, _ := s.Get(context.Background(), "Pipeline", commandspipeline.ExecutionName("servers"))
	if b.aborts != 1 || child.Metadata.DeletionTimestamp != nil {
		t.Fatal("did not wait for abort convergence")
	}
	reconcile(t, r, "cancel")
	child, _ = s.Get(context.Background(), "Pipeline", commandspipeline.ExecutionName("servers"))
	if child.Metadata.DeletionTimestamp != nil {
		t.Fatal("shared pipeline cleaned after abort")
	}
	if _, ok := s.Resources["Command/cancel"]; ok {
		t.Fatal("cancelled request retained")
	}
}
func TestExecutorSnapshotAndProviderIdentity(t *testing.T) {
	r, s, _, b := fixture(t)
	raw := addCommand(t, s, "snapshot", nil)
	reconcile(t, r, "snapshot")
	executor := s.Resources["CommandsPipeline/servers"]
	executor.Spec["inventoryCaptureGroupRef"] = map[string]any{"name": "changed"}
	s.Resources["CommandsPipeline/servers"] = executor
	reconcile(t, r, "snapshot")
	reconcile(t, r, "snapshot")
	readyChild(t, s, raw.Metadata.UID)
	child := s.Resources["Pipeline/"+commandspipeline.ExecutionName("servers")]
	if !strings.Contains(child.Spec["definition"].(map[string]any)["data"].(string), "INVENTORY_CAPTURE_GROUP: servers") {
		t.Fatal("accepted settings changed")
	}
	provider := s.Resources["PipelineProvider/concourse"]
	provider.Metadata.UID = "replaced"
	s.Resources["PipelineProvider/concourse"] = provider
	if err := r.Reconcile(context.Background(), controller.Request{Name: "snapshot"}); err == nil || b.triggers != 0 {
		t.Fatal("provider replacement was not blocked")
	}
}
func TestUnownedChildNeverDeletedOrOverwritten(t *testing.T) {
	r, s, _, b := fixture(t)
	raw := addCommand(t, s, "conflict", nil)
	reconcile(t, r, "conflict")
	reconcile(t, r, "conflict")
	_ = raw
	name := commandspipeline.ExecutionName("servers")
	s.Resources["Pipeline/"+name] = resource.Resource{APIVersion: resource.APIVersion, Kind: "Pipeline", Metadata: resource.Metadata{Name: name, UID: "other", ResourceVersion: "1"}, Spec: map[string]any{}}
	if err := r.Reconcile(context.Background(), controller.Request{Name: "conflict"}); err == nil || b.triggers != 0 {
		t.Fatal("unowned child overwritten")
	}
}

func TestRestartKeepsExecutorSlotAndRejectsAmbiguousSubmission(t *testing.T) {
	r, s, p, b := fixture(t)
	prepared(t, r, s, "uncertain")
	b.builds = []pipeline.Build{{ID: 10, Status: "succeeded"}}
	b.err = errors.New("lost response")
	reconcile(t, r, "uncertain")
	prepared(t, r, s, "waiting")
	restarted := NewReconcilerWithDependencies(s, r.executorConfigForTest(), p, b)
	reconcile(t, restarted, "waiting")
	if b.triggers != 1 {
		t.Fatal("slot was not durable")
	}
	b.builds = append(b.builds, pipeline.Build{ID: 100, Status: "started"}, pipeline.Build{ID: 101, Status: "pending"})
	if err := restarted.Reconcile(context.Background(), controller.Request{Name: "uncertain"}); err == nil {
		t.Fatal("ambiguous builds adopted")
	}
}

func TestRealGitInputsAreRemovedWithoutTouchingAnotherCommand(t *testing.T) {
	r, s, _, b := fixture(t)
	remote := filepath.Join(t.TempDir(), "commands.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}
	publisher := &gitpublication.GitPublisher{Now: time.Now}
	repository := s.Resources["GitRepository/repo"]
	repository.Spec["url"] = remote
	s.Resources["GitRepository/repo"] = repository
	if _, err := publisher.Publish(context.Background(), gitpublication.PublishRequest{Repository: apigen.GitRepositorySpec{Url: remote}, Branch: "main", RootPath: ".", PreserveUnmanaged: true, Artifacts: []gitpublication.Artifact{{Path: "README", Content: []byte("keep")}}}); err != nil {
		t.Fatal(err)
	}
	r.publisher = publisher
	first := prepared(t, r, s, "git-first")
	second := prepared(t, r, s, "git-second")
	reconcile(t, r, "git-first")
	b.builds[0].Status = "succeeded"
	reconcile(t, r, "git-first")
	current, _ := s.Get(context.Background(), "Command", "git-first")
	rv, _ := version(current.Metadata)
	if err := s.Delete(context.Background(), "Command", "git-first", rv); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "git-first")
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(commandspipeline.ExecutionName("servers")), true)
	if err != nil {
		t.Fatal(err)
	}
	commit, _ := repo.CommitObject(ref.Hash())
	tree, _ := commit.Tree()
	if _, err := tree.File(executionName(first.Metadata.UID) + "/run.sh"); err == nil {
		t.Fatal("first command input was retained")
	}
	if _, err := tree.File(executionName(second.Metadata.UID) + "/run.sh"); err != nil {
		t.Fatal("second command input was removed", err)
	}
	if _, err := tree.File("README"); err != nil {
		t.Fatal("unrelated Git content removed", err)
	}
}
