package etcd

import (
	"context"
	"strconv"

	"github.com/asdf57/stigmergy/internal/resource"
	storage "github.com/asdf57/stigmergy/internal/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// UpdateStatusPair changes only the supplied statuses, for both exact lifetimes/revisions.
func (s *Store) UpdateStatusPair(ctx context.Context, first, second resource.Resource) (resource.Resource, error) {
	values := []resource.Resource{first, second}
	comparisons := make([]clientv3.Cmp, 0, 2)
	writes := make([]clientv3.Op, 0, 2)
	for _, desired := range values {
		revision, err := strconv.ParseInt(desired.Metadata.ResourceVersion, 10, 64)
		if err != nil {
			return resource.Resource{}, err
		}
		current, err := s.Get(ctx, desired.Kind, desired.Metadata.Name)
		if err != nil {
			return resource.Resource{}, err
		}
		if current.Metadata.UID != desired.Metadata.UID || current.Metadata.ResourceVersion != desired.Metadata.ResourceVersion {
			return resource.Resource{}, storage.ErrConflict
		}
		current.Status = desired.Status
		current.Metadata.ResourceVersion = ""
		encoded, err := encode(current)
		if err != nil {
			return resource.Resource{}, err
		}
		key := s.keys.resource(current.Kind, current.Metadata.Name)
		comparisons = append(comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", revision))
		writes = append(writes, clientv3.OpPut(key, string(encoded)))
	}
	response, err := s.client.Txn(ctx).If(comparisons...).Then(writes...).Commit()
	if err != nil {
		return resource.Resource{}, err
	}
	if !response.Succeeded {
		return resource.Resource{}, storage.ErrConflict
	}
	first.Metadata.ResourceVersion = strconv.FormatInt(response.Header.Revision, 10)
	return first, nil
}
