# Controller Homework

This homework adds controller support in small stages.

Complete one assignment at a time. Do not start the next assignment until the tests pass.

## The basic idea

A controller does this work:

```text
A resource changes.
        |
        v
The controller receives its name.
        |
        v
The controller reads the newest resource.
        |
        v
The controller does the required work.
```

## Assignment 1: Follow one request

Goal: Understand the code that already exists.

- [ ] Start the API and etcd.
- [ ] Send one `MachineReport` request.
- [ ] Find the request route in `internal/api/routing.go`.
- [ ] Find the handler in `internal/api/handlers.go`.
- [ ] Find the etcd write in `internal/store/etcd/store.go`.
- [ ] Write one sentence about each file.

Finish condition: You can show the path from the HTTP request to etcd.

## Assignment 2: Define a controller

Goal: Add the smallest controller interface.

- [ ] Create `internal/controller/controller.go`.
- [ ] Add a `Request` type with `Kind` and `Name` fields.
- [ ] Add a `Reconciler` interface.
- [ ] Give the interface one `Reconcile` method.
- [ ] Add a short comment for each public type.
- [ ] Run `go test ./...`.

Use this shape:

```go
type Request struct {
	Kind string
	Name string
}

type Reconciler interface {
	Reconcile(context.Context, Request) error
}
```

Finish condition: The project builds, but no controller runs.

## Assignment 3: Make a practice controller

Goal: Run controller code without etcd watch support.

- [ ] Create `internal/controller/machinereport/controller.go`.
- [ ] Give the controller access to the resource store.
- [ ] Read one `MachineReport` in `Reconcile`.
- [ ] Write a log message with the resource name.
- [ ] Add a unit test with a fake store.
- [ ] Run `go test ./...`.

Do not change resources in this assignment.

Finish condition: A test calls `Reconcile`, and the controller reads the report.

## Assignment 4: Watch for changes

Goal: Let the etcd store report resource changes.

- [ ] Add a `Watch` method to the store interface.
- [ ] Add an event type with `Kind` and `Name` fields.
- [ ] Implement `Watch` in the etcd store.
- [ ] Test create, update, and delete events.
- [ ] Test that watch shutdown does not leak a goroutine.
- [ ] Run `go test ./...`.

Finish condition: A test changes a resource and receives its name from `Watch`.

## Assignment 5: Connect the watch to the controller

Goal: Run `Reconcile` after a resource changes.

- [ ] Create `internal/controller/manager.go`.
- [ ] Start a watch for `MachineReport` resources.
- [ ] Call the `MachineReport` reconciler for each event.
- [ ] Stop the watch when the application context stops.
- [ ] Start the manager from `cmd/homelab-controller/main.go`.
- [ ] Add a test for one event.
- [ ] Run `go test ./...`.

Use one worker. Do not add a complex work queue yet.

Finish condition: A changed report causes one reconcile operation.

## Assignment 6: Make retries safe

Goal: Do not lose work after a temporary error.

- [ ] Put failed requests back into the work channel.
- [ ] Wait before each retry.
- [ ] Stop retries when the application context stops.
- [ ] Do not run two copies of the same request at the same time.
- [ ] Add a test in which the first call fails and the second call succeeds.
- [ ] Run `go test ./...`.

Finish condition: A temporary error does not lose the resource change.

## Assignment 7: Add real controller work

Goal: Make the first useful controller decision.

Before you write code, complete these sentences:

```text
The controller watches ____________________.
The controller reads ______________________.
The controller creates or changes __________.
The controller is successful when __________.
```

Then complete this work:

- [ ] Write one test for the successful result.
- [ ] Write one test for a repeated reconcile operation.
- [ ] Write the controller code.
- [ ] Run `go test ./...`.

Finish condition: Two reconcile operations produce the same correct state.

## Rules for all assignments

- [ ] Keep each change small.
- [ ] Run tests after each change.
- [ ] Do not edit generated files.
- [ ] Do not add a feature for a future problem.
- [ ] Stop when the finish condition is true.

## Current assignment

Start with Assignment 1 only.

