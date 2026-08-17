package controller

import (
	"context"
	"log/slog"

	"github.com/asdf57/prov-controller-test/go/internal/store/etcd"
	"github.com/asdf57/prov-controller-test/go/internal/utils"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type Manager struct {
	log           *slog.Logger
	store         *etcd.Store
	registrations []Registration
	controllers   map[string]*Controller
	workqueue     utils.Queue
}

func NewManager(log *slog.Logger, store *etcd.Store, registrations []Registration) *Manager {
	log.Info("registering controllers", "count", len(registrations))

	controllerReconcilers := make(map[string]*Controller, len(registrations))
	for _, registration := range registrations {
		controllerReconcilers[registration.Name] = registration.Controller
	}

	return &Manager{
		log:           log,
		store:         store,
		registrations: registrations,
		controllers:   controllerReconcilers,
	}
}

func (m *Manager) Serve(ctx context.Context) error {
	m.log.Info("serving manager")
	resourceControllerWatchMap := make(map[string][]string)
	resourceWatchMap := make(map[string]<-chan clientv3.WatchResponse)

	// to-do: Add secondary watch support

	for _, registration := range m.registrations {
		m.log.Info("registering watch", "kind", registration.ReconcilesKind)
		resourceControllerWatchMap[registration.ReconcilesKind] = append(resourceControllerWatchMap[registration.ReconcilesKind], registration.Name)

		go func() {
			m.log.Info("starting controller", "name", registration.Name)
			registration.Controller.Run(ctx)
		}()
	}

	// For every relevant resource kind, create a new watch stream
	for kind, _ := range resourceControllerWatchMap {
		resourceWatchMap[kind] = m.store.Watch(ctx, kind)
	}

	// For passed etcd events, trigger the appropriate reconcilers
	for kind, events := range resourceWatchMap {
		resourceKind := kind
		eventsChannel := events
		controllersWatching := resourceControllerWatchMap[resourceKind]

		go func() {
			for event := range eventsChannel {
				requests, err := EtcdEventsToRequests(event)
				if err != nil {
					m.log.Error("could not convert etcd watch events", "kind", resourceKind, "error", err)
					continue
				}

				// Add the event to every relevant controller queue watching this resource kind
				for _, request := range requests {
					for _, controllerName := range controllersWatching {
						if err := m.controllers[controllerName].workqueue.Add(request); err != nil {
							m.log.Error("could not add watch", "kind", resourceKind, "name", controllerName, "error", err)
						}
					}
				}
			}
		}()
	}

	return nil
}
