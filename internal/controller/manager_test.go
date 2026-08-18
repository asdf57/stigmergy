package controller

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type fakeWatchStore struct {
	streams map[string]chan clientv3.WatchResponse
}

func (s *fakeWatchStore) Watch(_ context.Context, kind string) <-chan clientv3.WatchResponse {
	return s.streams[kind]
}

type recordingReconciler struct {
	requests chan Request
}

func (r *recordingReconciler) Reconcile(ctx context.Context, request Request) error {
	select {
	case r.requests <- request:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestManagerMapsSecondaryWatchToPrimaryRequest(t *testing.T) {
	primaryEvents := make(chan clientv3.WatchResponse)
	secondaryEvents := make(chan clientv3.WatchResponse, 1)
	watchStore := &fakeWatchStore{streams: map[string]chan clientv3.WatchResponse{
		"Machine":       primaryEvents,
		"MachineReport": secondaryEvents,
	}}
	reconciler := &recordingReconciler{requests: make(chan Request, 1)}
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		watchStore,
		[]Registration{
			{
				Name:       "machine-controller",
				Controller: NewController(reconciler),
				Watches: []Watch{
					{Kind: "Machine", Mapper: IdentityMapper},
					{
						Kind: "MachineReport",
						Mapper: func(_ context.Context, source Request) ([]Request, error) {
							return []Request{{Kind: "Machine", Name: "machine-for-" + source.Name}}, nil
						},
					},
				},
			},
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Serve(ctx); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	secondaryEvents <- watchResponseForKey("/homelab/v1/resources/MachineReport/alpha")

	select {
	case got := <-reconciler.requests:
		want := (Request{Kind: "Machine", Name: "machine-for-alpha"})
		if got != want {
			t.Fatalf("Reconcile() request = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("secondary watch event did not trigger reconciliation")
	}

	close(primaryEvents)
	close(secondaryEvents)
}

func TestManagerUsesIdentityMapperForPrimaryWatch(t *testing.T) {
	primaryEvents := make(chan clientv3.WatchResponse, 1)
	watchStore := &fakeWatchStore{streams: map[string]chan clientv3.WatchResponse{
		"MachineReport": primaryEvents,
	}}
	reconciler := &recordingReconciler{requests: make(chan Request, 1)}
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		watchStore,
		[]Registration{
			{
				Name:       "machine-report-controller",
				Controller: NewController(reconciler),
				Watches:    []Watch{{Kind: "MachineReport", Mapper: IdentityMapper}},
			},
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Serve(ctx); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	primaryEvents <- watchResponseForKey("/homelab/v1/resources/MachineReport/alpha")

	select {
	case got := <-reconciler.requests:
		want := (Request{Kind: "MachineReport", Name: "alpha"})
		if got != want {
			t.Fatalf("Reconcile() request = %#v, want %#v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("primary watch event did not trigger reconciliation")
	}

	close(primaryEvents)
}

func TestManagerRejectsWatchWithoutMapper(t *testing.T) {
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		&fakeWatchStore{streams: make(map[string]chan clientv3.WatchResponse)},
		[]Registration{
			{
				Name:       "machine-controller",
				Controller: NewController(&recordingReconciler{requests: make(chan Request, 1)}),
				Watches:    []Watch{{Kind: "Machine"}},
			},
		},
	)

	if err := manager.Serve(context.Background()); err == nil {
		t.Fatal("Serve() error = nil, want missing mapper error")
	}
}

func watchResponseForKey(key string) clientv3.WatchResponse {
	return clientv3.WatchResponse{
		Events: []*clientv3.Event{
			{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte(key)}},
		},
	}
}
