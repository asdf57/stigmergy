package utils

import (
	"context"
	"sync"
)

/*
   A work item represents a request to reconcile the latest state of a
   resource identified by Kind/Name. It does NOT represent an individual
   change event.

   Each controller owns an independent queue. Therefore, two controllers
   may reconcile the same resource concurrently and retry independently.

   Invariants:
   (1) Within one controller, a resource key may have at most one active
       reconciliation.
   (2) Repeated additions of an already queued key are coalesced (A, A, A => A)
   (3) If a key is added while it is being processed, exactly one additional
       reconciliation is requested (via dirty signaling)
   (4) A key cannot be queued and processing at the same time.
   (5) A failed reconciliation is retried according to the retry policy.
   (6) If a reconciliation both fails and becomes dirty, one subsequent
       reconciliation can satisfy both because it reads the latest state.

   Per-key states:
   - idle
   - queued
   - processing
   - processing + dirty

   "Dirty" means that another reconciliation was requested after the
   current reconciliation began.
*/

type Queue struct {
	mu         sync.Mutex
	ready      chan struct{}
	items      []WorkItem
	queued     map[string]bool // entries pending an execution
	processing map[string]bool // entries currently in-flight
	dirty      map[string]bool // entries blocked from running due to an in-flight entry
}

type WorkItem interface {
	Key() string
}

func NewQueue() *Queue {
	return &Queue{
		ready:      make(chan struct{}, 1),
		items:      make([]WorkItem, 0),
		queued:     make(map[string]bool),
		processing: make(map[string]bool),
		dirty:      make(map[string]bool),
	}
}

// signalLocked wakes a worker without waiting for one to be ready. The caller
// must hold q.mu.
func (q *Queue) signalLocked() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// addLocked adds a work item while q.mu is held.
func (q *Queue) addLocked(workItem WorkItem) error {
	// Axiom (2)
	if _, ok := q.queued[workItem.Key()]; ok {
		return nil
	}

	// Axioms (1), (3)
	if _, ok := q.processing[workItem.Key()]; ok {
		q.dirty[workItem.Key()] = true
		return nil
	}

	q.queued[workItem.Key()] = true
	q.items = append(q.items, workItem)
	q.signalLocked()

	return nil
}

func (q *Queue) Add(workItem WorkItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.addLocked(workItem)
}

func (q *Queue) Pop(ctx context.Context) (WorkItem, bool) {
	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.ready:
			q.mu.Lock()
			if len(q.items) == 0 {
				q.mu.Unlock()
				continue
			}

			workItem := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]

			delete(q.queued, workItem.Key())
			q.processing[workItem.Key()] = true
			if len(q.items) > 0 {
				q.signalLocked()
			}
			q.mu.Unlock()

			return workItem, true
		}
	}
}

func (q *Queue) Done(workItem WorkItem) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.processing[workItem.Key()]; ok {
		delete(q.processing, workItem.Key())
	}

	if _, ok := q.dirty[workItem.Key()]; ok {
		delete(q.dirty, workItem.Key())
		if err := q.addLocked(workItem); err != nil {
			return
		}
	}
}
