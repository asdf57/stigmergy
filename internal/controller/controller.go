package controller

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/asdf57/prov-controller-test/go/internal/utils"
	clientv3 "go.etcd.io/etcd/client/v3"
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

func (r Request) Key() string {
	return fmt.Sprintf("%s/%s", r.Kind, r.Name)
}

type Reconciler interface {
	Reconcile(context.Context, Request) error
}

type Controller struct {
	workqueue  *utils.Queue
	reconciler Reconciler
}

func NewController(reconciler Reconciler) *Controller {
	return &Controller{
		workqueue:  utils.NewQueue(),
		reconciler: reconciler,
	}
}

// Run is called by the manager. It has a consistent form across all controllers
func (c *Controller) Run(ctx context.Context) {
	for {
		item, ok := c.workqueue.Pop(ctx)
		if !ok {
			return
		}

		request := item.(Request)

		if err := c.reconciler.Reconcile(ctx, request); err != nil {
			_ = c.workqueue.Add(item)
		}

		c.workqueue.Done(item)
	}
}

// RequestMapper converts a watched resource into reconciliation requests.
type RequestMapper func(context.Context, Request) ([]Request, error)

// IdentityMapper reconciles the resource that produced the watch event.
func IdentityMapper(_ context.Context, request Request) ([]Request, error) {
	return []Request{request}, nil
}

type Watch struct {
	Kind string
	// Mapper converts a watched resource into reconciliation requests.
	Mapper RequestMapper
}

type Registration struct {
	Name       string
	Watches    []Watch
	Controller *Controller
}

func EtcdEventsToRequests(response clientv3.WatchResponse) ([]Request, error) {
	if err := response.Err(); err != nil {
		return nil, fmt.Errorf("etcd watch response: %w", err)
	}

	requests := make([]Request, 0, len(response.Events))
	for _, event := range response.Events {
		if event == nil || event.Kv == nil {
			return nil, fmt.Errorf("etcd watch response contains an event without a key-value")
		}

		key := strings.TrimSuffix(string(event.Kv.Key), "/")
		name := path.Base(key)
		kind := path.Base(path.Dir(key))
		if key == "" || name == "." || name == "/" || kind == "." || kind == "/" {
			return nil, fmt.Errorf("invalid resource key %q in etcd watch response", event.Kv.Key)
		}

		requests = append(requests, Request{Kind: kind, Name: name})
	}
	return requests, nil
}
