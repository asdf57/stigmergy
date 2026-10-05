// Package testutil provides an isolated resource store for controller tests.
package testutil

import (
	"context"
	"fmt"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"strconv"
	"time"
)

type Store struct {
	store.Store
	Resources map[string]resource.Resource
	Writes    int
}

func NewStore(values ...resource.Resource) *Store {
	s := &Store{Resources: map[string]resource.Resource{}}
	for _, value := range values {
		s.Resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return s
}
func (s *Store) Get(_ context.Context, kind, name string) (resource.Resource, error) {
	value, ok := s.Resources[kind+"/"+name]
	if !ok {
		return resource.Resource{}, store.ErrNotFound
	}
	return value, nil
}
func (s *Store) List(_ context.Context, kind string) (resource.List, error) {
	values := resource.List{}
	for _, value := range s.Resources {
		if value.Kind == kind {
			values.Items = append(values.Items, value)
		}
	}
	return values, nil
}
func (s *Store) Create(ctx context.Context, value resource.Resource) (resource.Resource, error) {
	if _, err := s.Get(ctx, value.Kind, value.Metadata.Name); err == nil {
		return resource.Resource{}, store.ErrConflict
	}
	value.Metadata.UID = fmt.Sprintf("uid-%s-%s", value.Kind, value.Metadata.Name)
	value.Metadata.ResourceVersion = "1"
	value.Metadata.Generation = 1
	s.Resources[value.Kind+"/"+value.Metadata.Name] = value
	s.Writes++
	return value, nil
}
func (s *Store) Update(ctx context.Context, value resource.Resource, version int64) (resource.Resource, error) {
	old, err := s.Get(ctx, value.Kind, value.Metadata.Name)
	if err != nil {
		return resource.Resource{}, err
	}
	if old.Metadata.ResourceVersion != strconv.FormatInt(version, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	value.Metadata.ResourceVersion = strconv.FormatInt(version+1, 10)
	if !resource.EqualJSON(value.Spec, old.Spec) {
		value.Metadata.Generation = old.Metadata.Generation + 1
	}
	value.Status = old.Status
	s.Writes++
	if value.Metadata.DeletionTimestamp != nil && len(value.Metadata.Finalizers) == 0 {
		delete(s.Resources, value.Kind+"/"+value.Metadata.Name)
	} else {
		s.Resources[value.Kind+"/"+value.Metadata.Name] = value
	}
	return value, nil
}
func (s *Store) UpdateStatus(ctx context.Context, kind, name string, status map[string]any, version int64) (resource.Resource, error) {
	value, err := s.Get(ctx, kind, name)
	if err != nil {
		return resource.Resource{}, err
	}
	if value.Metadata.ResourceVersion != strconv.FormatInt(version, 10) {
		return resource.Resource{}, store.ErrConflict
	}
	value.Status = status
	value.Metadata.ResourceVersion = strconv.FormatInt(version+1, 10)
	s.Writes++
	s.Resources[kind+"/"+name] = value
	return value, nil
}
func (s *Store) Delete(ctx context.Context, kind, name string, version int64) error {
	value, err := s.Get(ctx, kind, name)
	if err != nil {
		return err
	}
	if value.Metadata.ResourceVersion != strconv.FormatInt(version, 10) {
		return store.ErrConflict
	}
	if len(value.Metadata.Finalizers) == 0 {
		delete(s.Resources, kind+"/"+name)
	} else {
		now := time.Now()
		value.Metadata.DeletionTimestamp = &now
		value.Metadata.ResourceVersion = strconv.FormatInt(version+1, 10)
		s.Resources[kind+"/"+name] = value
	}
	return nil
}
