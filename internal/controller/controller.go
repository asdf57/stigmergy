package controller

import (
	"context"
	"time"

	"github.com/asdf57/prov-controller-test/go/internal/resource"
)

const (
	EventAdded    = "added"
	EventModified = "modified"
	EventDeleted  = "deleted"
)

type Event struct {
	Kind string
	Name string
	Type string
}

type Request struct {
	Kind string
	Name string
}

type Reconciler interface {
	Reconcile(context.Context, Request) error
}

type Watch struct {
	Kind string
}

type Registration struct {
	Name           string
	ReconcilesKind string
	Watches        []Watch
	ResyncPeriod   time.Duration
	Reconciler     Reconciler
}

// Store is an abstraction that takes in the etcd store and spits out
// Event objects (containing the Kind, name, and event type). This is
// the format that the ControllerManager can then accept and use.
type Store interface {
	Watch(context.Context, string, int64) (<-chan Event, error)
	List(context.Context, string) (resource.List, error)
}
