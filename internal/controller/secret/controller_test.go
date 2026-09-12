package secret

import (
	"context"
	"testing"
	"time"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type missingSecretStore struct {
	secret  resource.Resource
	updated resource.Resource
}

func (s *missingSecretStore) Create(context.Context, resource.Resource) (resource.Resource, error) {
	return resource.Resource{}, nil
}

func (s *missingSecretStore) Get(_ context.Context, kind, _ string) (resource.Resource, error) {
	if kind == registry.SecretResource.Kind {
		return s.secret, nil
	}
	return resource.Resource{}, store.ErrNotFound
}

func (*missingSecretStore) List(context.Context, string) (resource.List, error) {
	return resource.List{}, nil
}

func (s *missingSecretStore) Update(_ context.Context, candidate resource.Resource, _ int64) (resource.Resource, error) {
	s.updated = candidate
	return candidate, nil
}

func (*missingSecretStore) UpdateStatus(context.Context, string, string, map[string]any, int64) (resource.Resource, error) {
	return resource.Resource{}, nil
}

func (*missingSecretStore) Delete(context.Context, string, string, int64) error { return nil }

func (*missingSecretStore) DeleteCollection(context.Context, string) (int64, error) {
	return 0, nil
}

func (*missingSecretStore) Ready(context.Context) error { return nil }

func TestReconcileRemovesFinalizerWhenSecretStoreIsGone(t *testing.T) {
	deletionTimestamp := time.Now().UTC()
	secretResource := registry.NewSecret(resource.Metadata{
		Name:              "orphaned-secret",
		ResourceVersion:   "7",
		Generation:        1,
		DeletionTimestamp: &deletionTimestamp,
		Finalizers:        []string{cleanupFinalizer},
	}, apigen.SecretSpec{
		SecretStoreRef: apigen.SecretStoreReference{Name: "deleted-store"},
		Path:           "orphaned/secret",
		Data:           map[string]string{"value": "secret"},
	})
	raw, err := secretResource.Encode()
	if err != nil {
		t.Fatalf("encode test Secret: %v", err)
	}
	storage := &missingSecretStore{secret: raw}
	reconciler := NewReconciler(storage)

	if err := reconciler.Reconcile(context.Background(), controller.Request{
		Kind: registry.SecretResource.Kind,
		Name: secretResource.Metadata.Name,
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(storage.updated.Metadata.Finalizers) != 0 {
		t.Fatalf("updated finalizers = %#v, want none", storage.updated.Metadata.Finalizers)
	}
}
