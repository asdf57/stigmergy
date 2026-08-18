package controller

import (
	"context"
	"fmt"
	"log/slog"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type WatchStore interface {
	Watch(context.Context, string) <-chan clientv3.WatchResponse
}

type Manager struct {
	log           *slog.Logger
	store         WatchStore
	registrations []Registration
	controllers   map[string]*Controller
}

func NewManager(log *slog.Logger, store WatchStore, registrations []Registration) *Manager {
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

type watchTarget struct {
	controllerName string
	mapper         RequestMapper
}

func (m *Manager) Serve(ctx context.Context) error {
	m.log.Info("serving manager")
	watchTargetsByKind := make(map[string][]watchTarget)
	resourceWatchMap := make(map[string]<-chan clientv3.WatchResponse)

	for _, registration := range m.registrations {
		if registration.Controller == nil {
			return fmt.Errorf("controller registration %q has no controller", registration.Name)
		}

		for _, watch := range registration.Watches {
			if watch.Kind == "" {
				return fmt.Errorf("controller registration %q has a watch without a kind", registration.Name)
			}
			if watch.Mapper == nil {
				return fmt.Errorf("controller registration %q watch for %q has no mapper", registration.Name, watch.Kind)
			}

			m.log.Info("registering watch", "kind", watch.Kind, "controller", registration.Name)
			watchTargetsByKind[watch.Kind] = append(
				watchTargetsByKind[watch.Kind],
				watchTarget{
					controllerName: registration.Name,
					mapper:         watch.Mapper,
				},
			)
		}
	}

	for _, registration := range m.registrations {
		go func() {
			m.log.Info("starting controller", "name", registration.Name)
			registration.Controller.Run(ctx)
		}()
	}

	// For every relevant resource kind, create a new watch stream
	for kind := range watchTargetsByKind {
		resourceWatchMap[kind] = m.store.Watch(ctx, kind)
	}

	// For passed etcd events, trigger the appropriate reconcilers
	for kind, events := range resourceWatchMap {
		resourceKind := kind
		eventsChannel := events
		watchTargets := watchTargetsByKind[resourceKind]

		go func() {
			for event := range eventsChannel {
				requests, err := EtcdEventsToRequests(event)
				if err != nil {
					m.log.Error("could not convert etcd watch events", "kind", resourceKind, "error", err)
					continue
				}

				for _, sourceRequest := range requests {
					for _, target := range watchTargets {
						mappedRequests, err := target.mapper(ctx, sourceRequest)
						if err != nil {
							m.log.Error("could not map watched resource", "kind", resourceKind, "name", sourceRequest.Name, "controller", target.controllerName, "error", err)
							continue
						}

						for _, request := range mappedRequests {
							if err := m.controllers[target.controllerName].workqueue.Add(request); err != nil {
								m.log.Error("could not enqueue request", "kind", request.Kind, "name", request.Name, "controller", target.controllerName, "error", err)
							}
						}
					}
				}
			}
		}()
	}

	return nil
}
