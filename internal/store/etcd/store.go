package etcd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/asdf57/prov-controller-test/go/internal/resource"
	storage "github.com/asdf57/prov-controller-test/go/internal/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type Store struct {
	client *clientv3.Client
	keys   keyspace
	now    func() time.Time
	newUID func() (string, error)
}

func New(client *clientv3.Client, prefix string) *Store {
	return &Store{
		client: client,
		keys:   newKeyspace(prefix),
		now:    time.Now,
		newUID: randomUID,
	}
}

func (s *Store) Watch(ctx context.Context, kind string) <-chan clientv3.WatchResponse {
	return s.client.Watch(ctx, s.keys.resourcePrefix(kind), clientv3.WithPrefix())
}

func (s *Store) Create(ctx context.Context, candidate resource.Resource) (resource.Resource, error) {
	uid, err := s.newUID()
	if err != nil {
		return resource.Resource{}, fmt.Errorf("generate resource UID: %w", err)
	}

	candidate.Metadata.UID = uid
	candidate.Metadata.Generation = 1
	candidate.Metadata.CreationTimestamp = s.now().UTC()
	candidate.Metadata.ResourceVersion = ""

	value, err := encode(candidate)
	if err != nil {
		return resource.Resource{}, err
	}

	resourceKey := s.keys.resource(candidate.Kind, candidate.Metadata.Name)
	uidKey := s.keys.uid(uid)
	response, err := s.client.Txn(ctx).
		If(
			clientv3.Compare(clientv3.Version(resourceKey), "=", 0),
			clientv3.Compare(clientv3.Version(uidKey), "=", 0),
		).
		Then(
			clientv3.OpPut(resourceKey, string(value)),
			clientv3.OpPut(uidKey, resourceKey),
		).
		Commit()
	if err != nil {
		return resource.Resource{}, fmt.Errorf("create resource: %w", err)
	}
	if !response.Succeeded {
		return resource.Resource{}, fmt.Errorf("%w: %s %q already exists", storage.ErrConflict, candidate.Kind, candidate.Metadata.Name)
	}

	candidate.Metadata.ResourceVersion = strconv.FormatInt(response.Header.Revision, 10)
	return candidate, nil
}

func (s *Store) Get(ctx context.Context, kind, name string) (resource.Resource, error) {
	response, err := s.client.Get(ctx, s.keys.resource(kind, name))
	if err != nil {
		return resource.Resource{}, fmt.Errorf("get resource: %w", err)
	}
	if len(response.Kvs) == 0 {
		return resource.Resource{}, fmt.Errorf("%w: %s %q", storage.ErrNotFound, kind, name)
	}
	return decode(response.Kvs[0].Value, response.Kvs[0].ModRevision)
}

func (s *Store) List(ctx context.Context, kind string) (resource.List, error) {
	response, err := s.client.Get(
		ctx,
		s.keys.resourcePrefix(kind),
		clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend),
	)
	if err != nil {
		return resource.List{}, fmt.Errorf("list resources: %w", err)
	}

	items := make([]resource.Resource, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		item, err := decode(kv.Value, kv.ModRevision)
		if err != nil {
			return resource.List{}, err
		}
		items = append(items, item)
	}

	return resource.List{
		APIVersion: resource.APIVersion,
		Kind:       kind + "List",
		Metadata:   resource.ListMetadata{ResourceVersion: strconv.FormatInt(response.Header.Revision, 10)},
		Items:      items,
	}, nil
}

func (s *Store) Update(ctx context.Context, candidate resource.Resource, expectedRevision int64) (resource.Resource, error) {
	existing, err := s.Get(ctx, candidate.Kind, candidate.Metadata.Name)
	if err != nil {
		return resource.Resource{}, err
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(expectedRevision, 10) {
		return resource.Resource{}, fmt.Errorf("%w: expected %d, current %s", storage.ErrConflict, expectedRevision, existing.Metadata.ResourceVersion)
	}
	if candidate.Metadata.UID != "" && candidate.Metadata.UID != existing.Metadata.UID {
		return resource.Resource{}, fmt.Errorf("%w: metadata.uid is immutable", storage.ErrConflict)
	}

	candidate.Metadata.UID = existing.Metadata.UID
	candidate.Metadata.CreationTimestamp = existing.Metadata.CreationTimestamp
	candidate.Metadata.DeletionTimestamp = existing.Metadata.DeletionTimestamp
	candidate.Metadata.Generation = existing.Metadata.Generation
	candidate.Metadata.ResourceVersion = ""
	candidate.Status = existing.Status
	if !reflect.DeepEqual(candidate.Spec, existing.Spec) {
		candidate.Metadata.Generation++
	}

	value, err := encode(candidate)
	if err != nil {
		return resource.Resource{}, err
	}
	resourceKey := s.keys.resource(candidate.Kind, candidate.Metadata.Name)
	response, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(resourceKey), "=", expectedRevision)).
		Then(clientv3.OpPut(resourceKey, string(value))).
		Commit()
	if err != nil {
		return resource.Resource{}, fmt.Errorf("update resource: %w", err)
	}
	if !response.Succeeded {
		return resource.Resource{}, fmt.Errorf("%w: %s %q changed", storage.ErrConflict, candidate.Kind, candidate.Metadata.Name)
	}

	candidate.Metadata.ResourceVersion = strconv.FormatInt(response.Header.Revision, 10)
	return candidate, nil
}

func (s *Store) Delete(ctx context.Context, kind, name string, expectedRevision int64) error {
	existing, err := s.Get(ctx, kind, name)
	if err != nil {
		return err
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(expectedRevision, 10) {
		return fmt.Errorf("%w: expected %d, current %s", storage.ErrConflict, expectedRevision, existing.Metadata.ResourceVersion)
	}

	resourceKey := s.keys.resource(kind, name)
	response, err := s.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(resourceKey), "=", expectedRevision)).
		Then(
			clientv3.OpDelete(resourceKey),
			clientv3.OpDelete(s.keys.uid(existing.Metadata.UID)),
		).
		Commit()
	if err != nil {
		return fmt.Errorf("delete resource: %w", err)
	}
	if !response.Succeeded {
		return fmt.Errorf("%w: %s %q changed", storage.ErrConflict, kind, name)
	}
	return nil
}

func (s *Store) DeleteCollection(ctx context.Context, kind string) (int64, error) {
	response, err := s.client.Delete(
		ctx,
		s.keys.resourcePrefix(kind),
		clientv3.WithPrefix(),
		clientv3.WithPrevKV(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete resource collection: %w", err)
	}

	uidDeletes := make([]clientv3.Op, 0, len(response.PrevKvs))
	for _, previous := range response.PrevKvs {
		deleted, err := decode(previous.Value, previous.ModRevision)
		if err != nil {
			return response.Deleted, fmt.Errorf("clean up deleted resource UID: %w", err)
		}
		if deleted.Metadata.UID != "" {
			uidDeletes = append(uidDeletes, clientv3.OpDelete(s.keys.uid(deleted.Metadata.UID)))
		}
	}
	const cleanupBatchSize = 64
	for start := 0; start < len(uidDeletes); start += cleanupBatchSize {
		end := min(start+cleanupBatchSize, len(uidDeletes))
		if _, err := s.client.Txn(ctx).Then(uidDeletes[start:end]...).Commit(); err != nil {
			return response.Deleted, fmt.Errorf("clean up deleted resource UIDs: %w", err)
		}
	}
	return response.Deleted, nil
}

func (s *Store) Ready(ctx context.Context) error {
	_, err := s.client.Get(ctx, s.keys.prefix, clientv3.WithLimit(1))
	if err != nil {
		return fmt.Errorf("etcd readiness: %w", err)
	}
	return nil
}

func encode(value resource.Resource) ([]byte, error) {
	value.Metadata.ResourceVersion = ""
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode resource: %w", err)
	}
	return encoded, nil
}

func decode(value []byte, revision int64) (resource.Resource, error) {
	var decoded resource.Resource
	if err := json.Unmarshal(value, &decoded); err != nil {
		return resource.Resource{}, fmt.Errorf("decode resource: %w", err)
	}
	decoded.Metadata.ResourceVersion = strconv.FormatInt(revision, 10)
	return decoded, nil
}

func randomUID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
