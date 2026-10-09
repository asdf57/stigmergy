package iso

import (
	"context"
	"errors"
	"fmt"
	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/gitpublication"
	"github.com/asdf57/stigmergy/internal/isobuild"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/sshtrust"
	"github.com/asdf57/stigmergy/internal/store"
	"slices"
	"strconv"
	"strings"
)

const ownerAnnotation = "homelab.io/iso-uid"
const cleanupFinalizer = "homelab.io/iso-cleanup"

type Reconciler struct {
	store     store.Store
	publisher gitpublication.Publisher
	manifests isobuild.ManifestReader
	config    isobuild.Config
}

func NewReconciler(s store.Store, p gitpublication.Publisher, m isobuild.ManifestReader, c isobuild.Config) *Reconciler {
	return &Reconciler{store: s, publisher: p, manifests: m, config: c}
}
func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.ISOResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	image, err := registry.ISOResource.Decode(raw)
	if err != nil {
		return err
	}
	if image.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, image)
	}
	status := &apigen.ISOStatus{}
	if image.Status != nil {
		*status = *image.Status
	}
	fail := func(phase, reason, message string) error {
		return r.setStatus(ctx, image, status, phase, reason, message)
	}
	if _, err := gitpublication.SafePath(image.Spec.BuildInputs.Path); err != nil {
		return fail("Failed", "InvalidInputPath", err.Error())
	}
	// Publication destinations stay fixed for a resource lifetime. Recreate the
	// ISO to move its owned directory; do not strand previously generated inputs.
	if status.PublishedPath != nil && (*status.PublishedPath != image.Spec.BuildInputs.Path || status.PublishedBranch == nil || *status.PublishedBranch != image.Spec.BuildInputs.Branch || status.RepositoryRef == nil || status.RepositoryRef.Name != image.Spec.BuildInputs.RepositoryRef.Name) {
		return fail("Conflict", "PublicationDestinationChanged", "Create another ISO to change its Git destination")
	}
	dependencies := []struct {
		kind  string
		spec  apigen.ISOReference
		prior *apigen.ResourceReference
	}{
		{registry.SSHCertificateAuthorityResource.Kind, image.Spec.SshCertificateAuthorityRef, status.AuthorityRef},
		{registry.GitRepositoryResource.Kind, image.Spec.BuildInputs.RepositoryRef, status.RepositoryRef},
		{registry.PipelineProviderResource.Kind, image.Spec.PipelineProviderRef, status.ProviderRef},
	}
	resolved := make([]resource.Resource, 0, 3)
	for _, dependency := range dependencies {
		value, err := r.store.Get(ctx, dependency.kind, dependency.spec.Name)
		if errors.Is(err, store.ErrNotFound) {
			return fail("Pending", "DependencyNotFound", fmt.Sprintf("%s %q does not exist", dependency.kind, dependency.spec.Name))
		}
		if err != nil {
			return err
		}
		expected := ""
		if dependency.spec.Uid != nil {
			expected = *dependency.spec.Uid
		} else if dependency.prior != nil && dependency.prior.Name == dependency.spec.Name {
			expected = dependency.prior.Uid
		}
		if expected != "" && expected != value.Metadata.UID {
			return fail("Conflict", "DependencyIdentityChanged", "Explicitly bind the replacement dependency UID")
		}
		if value.Metadata.DeletionTimestamp != nil {
			return fail("Pending", "DependencyTerminating", dependency.kind+" is terminating")
		}
		resolved = append(resolved, value)
	}
	authority, err := registry.SSHCertificateAuthorityResource.Decode(resolved[0])
	if err != nil {
		return err
	}
	repository, err := registry.GitRepositoryResource.Decode(resolved[1])
	if err != nil {
		return err
	}
	provider, err := registry.PipelineProviderResource.Decode(resolved[2])
	if err != nil {
		return err
	}
	status.AuthorityRef = &apigen.ResourceReference{Name: authority.Metadata.Name, Uid: authority.Metadata.UID}
	status.RepositoryRef = &apigen.ResourceReference{Name: repository.Metadata.Name, Uid: repository.Metadata.UID}
	status.ProviderRef = &apigen.ResourceReference{Name: provider.Metadata.Name, Uid: provider.Metadata.UID}
	if authority.Status == nil || authority.Status.Phase == nil || *authority.Status.Phase != "Ready" || authority.Status.ObservedGeneration == nil || *authority.Status.ObservedGeneration != authority.Metadata.Generation || authority.Status.TrustBundle == nil || authority.Status.TrustBundleDigest == nil {
		return fail("Pending", "AuthorityNotReady", "The desired authority trust has not been resolved")
	}
	keys := []string{}
	for _, key := range *authority.Status.TrustBundle {
		keys = append(keys, key.PublicKey)
	}
	bundle, digest, err := sshtrust.Bundle(keys)
	if err != nil {
		return fail("Failed", "InvalidTrustBundle", err.Error())
	}
	if digest != *authority.Status.TrustBundleDigest {
		return fail("Failed", "TrustDigestMismatch", "Authority bundle and digest do not match")
	}
	status.DesiredTrustBundleDigest = &digest
	if provider.Spec.Type != apigen.PipelineProviderSpecTypeConcourse {
		return fail("Failed", "UnsupportedProvider", "Only Concourse ISO rendering is implemented")
	}
	if provider.Status == nil || provider.Status.Phase == nil || *provider.Status.Phase != "Ready" || provider.Status.ObservedGeneration == nil || *provider.Status.ObservedGeneration != provider.Metadata.Generation {
		return fail("Pending", "ProviderNotReady", "The pipeline provider is not Ready")
	}
	input, err := isobuild.Inputs(image, digest, r.config)
	if err != nil {
		return fail("Failed", "UnsupportedRecipe", err.Error())
	}
	keyPath := ""
	if repository.Spec.Authentication != nil {
		key, err := r.store.Get(ctx, registry.SSHKeyPairResource.Kind, repository.Spec.Authentication.SshKeyPairRef)
		if errors.Is(err, store.ErrNotFound) {
			return fail("Pending", "GitKeyNotFound", "Git authentication key does not exist")
		}
		if err != nil {
			return err
		}
		pair, err := registry.SSHKeyPairResource.Decode(key)
		if err != nil {
			return err
		}
		keyPath = pair.Spec.Path
	}
	definition, err := isobuild.RenderConcourse(image, repository, keyPath, r.config)
	if err != nil {
		return fail("Pending", "ConfigurationUnavailable", err.Error())
	}
	if err := r.checkOwnership(ctx, image); err != nil {
		return fail("Conflict", "InputOwnershipConflict", err.Error())
	}
	// The transport checks no-op content too, so external deletion or corruption
	// is repaired without status-only events making new commits.
	result, err := r.publisher.Publish(ctx, gitpublication.PublishRequest{Repository: repository.Spec, PublicationName: image.Metadata.Name, Branch: image.Spec.BuildInputs.Branch, RootPath: image.Spec.BuildInputs.Path, OwnerUID: image.Metadata.UID, Artifacts: []gitpublication.Artifact{{Path: "image.yaml", Content: []byte(input)}, {Path: "ssh-user-ca.pub", Content: []byte(bundle)}}})
	if err != nil {
		return fail("Pending", "InputPublicationFailed", err.Error())
	}
	status.InputRevision = &result.Revision
	status.PublishedInputContent = &input
	status.PublishedBranch = &image.Spec.BuildInputs.Branch
	status.PublishedPath = &image.Spec.BuildInputs.Path
	child, err := r.ensurePipeline(ctx, image, definition)
	if err != nil {
		return fail("Conflict", "PipelineOwnershipConflict", err.Error())
	}
	status.PipelineRef = &apigen.ResourceReference{Name: child.Metadata.Name, Uid: child.Metadata.UID}
	if child.Status == nil || child.Status.Phase == nil || *child.Status.Phase != "Ready" || child.Status.ObservedGeneration == nil || *child.Status.ObservedGeneration != child.Metadata.Generation {
		return fail("Pending", "PipelinePending", "Build inputs are published; the generated Pipeline is awaiting configuration")
	}
	manifest, err := r.manifests.Latest(ctx, image.Metadata.UID, input, digest)
	if err != nil {
		return fail("Pending", "ArtifactDiscoveryFailed", err.Error())
	}
	if manifest == nil {
		return fail("Pending", "BuildPending", "Pipeline is configured; no matching completed artifact manifest exists")
	}
	status.Artifacts = &manifest.Artifacts
	for _, artifact := range manifest.Artifacts {
		if artifact.Type == "rootfs" {
			arguments, err := isobuild.BootArguments(string(image.Spec.Distribution), artifact.Url)
			if err != nil {
				return fail("Failed", "InvalidBootRecipe", err.Error())
			}
			status.BootArguments = &arguments
		}
	}
	status.CompletedBuild = &apigen.ISOCompletedBuild{Id: manifest.BuildID, StartedAt: manifest.BuildStartedAt, InputRevision: manifest.InputRevision, SourceRevisions: manifest.SourceRevisions, TrustBundleDigest: manifest.TrustBundleDigest}
	return r.setStatus(ctx, image, status, "Ready", "ArtifactsAvailable", "A completed immutable build matches the desired public inputs")
}

func (r *Reconciler) checkOwnership(ctx context.Context, image registry.ISO) error {
	overlap := func(left, right string) bool {
		return left == "." || right == "." || left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
	}
	images, err := r.store.List(ctx, registry.ISOResource.Kind)
	if err != nil {
		return err
	}
	for _, raw := range images.Items {
		if raw.Metadata.UID == image.Metadata.UID {
			continue
		}
		other, err := registry.ISOResource.Decode(raw)
		if err != nil {
			return err
		}
		if other.Spec.BuildInputs.RepositoryRef.Name == image.Spec.BuildInputs.RepositoryRef.Name && other.Spec.BuildInputs.Branch == image.Spec.BuildInputs.Branch && overlap(other.Spec.BuildInputs.Path, image.Spec.BuildInputs.Path) {
			return fmt.Errorf("ISO %q owns an overlapping path", other.Metadata.Name)
		}
	}
	publications, err := r.store.List(ctx, registry.InventoryPublicationResource.Kind)
	if err != nil {
		return err
	}
	for _, raw := range publications.Items {
		other, err := registry.InventoryPublicationResource.Decode(raw)
		if err != nil {
			return err
		}
		if other.Spec.DestinationRef.Kind == "GitRepository" && other.Spec.DestinationRef.Name == image.Spec.BuildInputs.RepositoryRef.Name && other.Spec.Target.Branch == image.Spec.BuildInputs.Branch && overlap(other.Spec.Target.RootPath, image.Spec.BuildInputs.Path) {
			return fmt.Errorf("InventoryPublication %q owns an overlapping path", other.Metadata.Name)
		}
	}
	return nil
}
func (r *Reconciler) ensurePipeline(ctx context.Context, image registry.ISO, definition string) (registry.Pipeline, error) {
	name := "iso-" + image.Metadata.Name
	raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, name)
	missing := errors.Is(err, store.ErrNotFound)
	if err != nil && !missing {
		return registry.Pipeline{}, err
	}
	metadata := resource.Metadata{Name: name, Finalizers: []string{"homelab.io/pipeline-cleanup"}, Annotations: map[string]string{ownerAnnotation: image.Metadata.UID}}
	if !missing {
		if raw.Metadata.Annotations[ownerAnnotation] != image.Metadata.UID || raw.Metadata.DeletionTimestamp != nil {
			return registry.Pipeline{}, fmt.Errorf("refusing to adopt or modify Pipeline %q", name)
		}
		if image.Status != nil && image.Status.PipelineRef != nil && image.Status.PipelineRef.Uid != raw.Metadata.UID {
			return registry.Pipeline{}, fmt.Errorf("owned Pipeline was replaced unexpectedly")
		}
		metadata = raw.Metadata
	}
	desired := registry.NewPipeline(metadata, apigen.PipelineSpec{ProviderRef: apigen.PipelineProviderReference{Name: image.Spec.PipelineProviderRef.Name}, ExternalName: name, Definition: apigen.PipelineDefinition{Format: apigen.PipelineDefinitionFormatConcourse, Data: definition}})
	encoded, err := desired.Encode()
	if err != nil {
		return registry.Pipeline{}, err
	}
	if missing {
		raw, err = r.store.Create(ctx, encoded)
	} else if !resource.EqualJSON(raw.Spec, encoded.Spec) {
		version, parseErr := strconv.ParseInt(raw.Metadata.ResourceVersion, 10, 64)
		if parseErr != nil {
			return registry.Pipeline{}, parseErr
		}
		raw, err = r.store.Update(ctx, encoded, version)
	}
	if err != nil {
		return registry.Pipeline{}, err
	}
	return registry.PipelineResource.Decode(raw)
}
func (r *Reconciler) setStatus(ctx context.Context, image registry.ISO, status *apigen.ISOStatus, phase, reason, message string) error {
	p, g := apigen.ISOStatusPhase(phase), image.Metadata.Generation
	state := apigen.ISOConditionStatusFalse
	if phase == "Ready" {
		state = apigen.ISOConditionStatusTrue
	}
	conditions := []apigen.ISOCondition{{Type: "Ready", Status: state, Reason: reason, Message: &message, ObservedGeneration: &g}}
	status.Phase, status.ObservedGeneration, status.Conditions = &p, &g, &conditions
	if resource.EqualJSON(image.Status, status) {
		return nil
	}
	version, err := strconv.ParseInt(image.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	encoded, err := registry.ISOResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, image.Kind, image.Metadata.Name, encoded, version)
	return err
}
func (r *Reconciler) finalize(ctx context.Context, image registry.ISO) error {
	if !slices.Contains(image.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	servers, err := r.store.List(ctx, registry.ServerResource.Kind)
	if err != nil {
		return err
	}
	for _, raw := range servers.Items {
		server, err := registry.ServerResource.Decode(raw)
		if err != nil {
			return err
		}
		if server.Spec.Boot != nil && server.Spec.Boot.IsoRef.Name == image.Metadata.Name {
			return fmt.Errorf("ISO is still referenced by Server/%s", server.Metadata.Name)
		}
	}
	raw, err := r.store.Get(ctx, registry.PipelineResource.Kind, "iso-"+image.Metadata.Name)
	if err == nil {
		if raw.Metadata.Annotations[ownerAnnotation] != image.Metadata.UID {
			return fmt.Errorf("refusing to delete another owner's Pipeline")
		}
		if raw.Metadata.DeletionTimestamp == nil {
			version, e := strconv.ParseInt(raw.Metadata.ResourceVersion, 10, 64)
			if e != nil {
				return e
			}
			return r.store.Delete(ctx, raw.Kind, raw.Metadata.Name, version)
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// Check the current spec too: a crash can occur after publication but before
	// recording status. The Git owner marker prevents deleting unrelated files.
	reference := image.Spec.BuildInputs.RepositoryRef
	branch, path := image.Spec.BuildInputs.Branch, image.Spec.BuildInputs.Path
	if image.Status != nil && image.Status.PublishedPath != nil {
		if image.Status.RepositoryRef == nil || image.Status.PublishedBranch == nil {
			return fmt.Errorf("incomplete publication status; refusing unsafe cleanup")
		}
		reference.Name = image.Status.RepositoryRef.Name
		reference.Uid = &image.Status.RepositoryRef.Uid
		branch, path = *image.Status.PublishedBranch, *image.Status.PublishedPath
	}
	raw, err = r.store.Get(ctx, registry.GitRepositoryResource.Kind, reference.Name)
	if err != nil {
		return fmt.Errorf("cannot clean owned Git inputs: %w", err)
	}
	if reference.Uid != nil && raw.Metadata.UID != *reference.Uid {
		return fmt.Errorf("refusing cleanup through a replaced GitRepository")
	}
	repository, err := registry.GitRepositoryResource.Decode(raw)
	if err != nil {
		return err
	}
	_, err = r.publisher.Publish(ctx, gitpublication.PublishRequest{Repository: repository.Spec, PublicationName: image.Metadata.Name, Branch: branch, RootPath: path, OwnerUID: image.Metadata.UID, RemoveOwned: true})
	if err != nil {
		return err
	}
	image.Metadata.Finalizers = slices.DeleteFunc(image.Metadata.Finalizers, func(value string) bool { return value == cleanupFinalizer })
	encoded, err := image.Encode()
	if err != nil {
		return err
	}
	version, err := strconv.ParseInt(image.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, encoded, version)
	return err
}
func (r *Reconciler) RequestsForDependency(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	images, err := r.store.List(ctx, registry.ISOResource.Kind)
	if err != nil {
		return nil, err
	}
	requests := []controller.Request{}
	for _, image := range images.Items {
		requests = append(requests, controller.Request{Kind: image.Kind, Name: image.Metadata.Name})
	}
	return requests, nil
}
