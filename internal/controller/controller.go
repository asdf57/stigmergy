package controller

import "context"

const (
	EventAdded    = "added"
	EventModified = "modified"
	EventDeleted  = "deleted"
)

type Event struct {
	Type string
	Kind string
	Name string
}

type Store interface {
	// Current operations...
	Watch(context.Context, string, int64) (<-chan Event, error)
}
