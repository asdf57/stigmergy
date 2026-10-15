package testutil

import (
	"context"
	"strconv"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

// UpdateStatusPair models the same two-object revision/lifetime fence as etcd.
func (s *Store) UpdateStatusPair(ctx context.Context, first, second resource.Resource) (resource.Resource, error) {
	for _, desired := range []resource.Resource{first, second} {
		current, err := s.Get(ctx, desired.Kind, desired.Metadata.Name)
		if err != nil {
			return resource.Resource{}, err
		}
		if current.Metadata.UID != desired.Metadata.UID || current.Metadata.ResourceVersion != desired.Metadata.ResourceVersion {
			return resource.Resource{}, store.ErrConflict
		}
	}
	for index, desired := range []resource.Resource{first, second} {
		revision, _ := strconv.ParseInt(desired.Metadata.ResourceVersion, 10, 64)
		current, _ := s.Get(ctx, desired.Kind, desired.Metadata.Name)
		current.Status = desired.Status
		current.Metadata.ResourceVersion = strconv.FormatInt(revision+1, 10)
		s.Resources[current.Kind+"/"+current.Metadata.Name] = current
		if index == 0 {
			first = current
		}
	}
	s.Writes++
	return first, nil
}
