package store

import (
	"context"
	"errors"

	"github.com/asdf57/stigmergy/internal/resource"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource version conflict")
)

type Store interface {
	Create(context.Context, resource.Resource) (resource.Resource, error)
	Get(ctx context.Context, kind string, name string) (resource.Resource, error)
	List(context.Context, string) (resource.List, error)
	Update(context.Context, resource.Resource, int64) (resource.Resource, error)
	UpdateStatus(context.Context, string, string, map[string]any, int64) (resource.Resource, error)
	Delete(context.Context, string, string, int64) error
	DeleteCollection(context.Context, string) (int64, error)
	Ready(context.Context) error
}
