package controller

import (
	"context"
)

type Manager struct {
	store         Store
	registrations []Registration
}

func NewManager(store Store, registrations []Registration) *Manager {
	return &Manager{
		store:         store,
		registrations: registrations,
	}
}

func (m *Manager) Serve(ctx context.Context) error {
	return nil
}
