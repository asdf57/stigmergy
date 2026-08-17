package utils

import (
	"context"
	"testing"
	"time"
)

type testWorkItem struct {
	key string
}

func (w testWorkItem) Key() string {
	return w.key
}

func TestPopMovesItemFromQueuedToProcessing(t *testing.T) {
	q := NewQueue()
	item := testWorkItem{key: "MachineReport/alpha"}
	addDone := make(chan error, 1)

	go func() {
		addDone <- q.Add(item)
	}()

	got, ok := q.Pop(context.Background())
	if !ok {
		t.Fatal("Pop() reported cancellation, want an item")
	}
	if got.Key() != item.Key() {
		t.Fatalf("Pop() key = %q, want %q", got.Key(), item.Key())
	}

	if err := <-addDone; err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[item.Key()] {
		t.Errorf("Pop() left %q queued", item.Key())
	}
	if !q.processing[item.Key()] {
		t.Errorf("Pop() did not mark %q processing", item.Key())
	}
}

func TestPopReturnsWhenContextIsCanceled(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	item, ok := q.Pop(ctx)
	if ok {
		t.Fatalf("Pop() returned ok = true with item %#v, want cancellation", item)
	}
	if item != nil {
		t.Fatalf("Pop() item = %#v, want nil", item)
	}
}

func TestDoneRequeuesItemAddedWhileProcessing(t *testing.T) {
	q := NewQueue()
	item := testWorkItem{key: "MachineReport/alpha"}
	initialAddDone := make(chan error, 1)

	// idle -> queued
	go func() {
		initialAddDone <- q.Add(item)
	}()

	// queued -> processing
	first, ok := q.Pop(context.Background())
	if !ok {
		t.Fatal("first Pop() reported cancellation, want an item")
	}
	if first.Key() != item.Key() {
		t.Fatalf("first Pop() key = %q, want %q", first.Key(), item.Key())
	}
	if err := <-initialAddDone; err != nil {
		t.Fatalf("initial Add() error = %v", err)
	}

	// processing -> processing + dirty
	if err := q.Add(item); err != nil {
		t.Fatalf("Add() while processing error = %v", err)
	}

	// processing + dirty -> queued. Done must not wait for the next Pop.
	doneReturned := make(chan struct{})
	go func() {
		q.Done(item)
		close(doneReturned)
	}()

	select {
	case <-doneReturned:
	case <-time.After(time.Second):
		t.Fatal("Done() blocked while requeueing a dirty item")
	}

	// queued -> processing for the single coalesced rerun.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, ok := q.Pop(ctx)
	if !ok {
		t.Fatal("second Pop() did not return the dirty item")
	}
	if second.Key() != item.Key() {
		t.Fatalf("second Pop() key = %q, want %q", second.Key(), item.Key())
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued[item.Key()] {
		t.Errorf("second Pop() left %q queued", item.Key())
	}
	if !q.processing[item.Key()] {
		t.Errorf("second Pop() did not mark %q processing", item.Key())
	}
	if q.dirty[item.Key()] {
		t.Errorf("Done() left %q dirty after requeueing it", item.Key())
	}
}
