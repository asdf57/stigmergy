package api

import (
	"context"
	"fmt"
	"time"

	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

func operationTerminal(status map[string]any) bool {
	return status["phase"] == "Succeeded" || status["phase"] == "Failed"
}

func (s *Server) operationResource(ctx context.Context, reference map[string]any, target bool) (resource.Resource, error) {
	kind, name := fmt.Sprint(reference["kind"]), fmt.Sprint(reference["name"])
	for _, definition := range registry.Definitions {
		if definition.Kind != kind {
			continue
		}
		if target && (kind == "Operation" || definition.StatusSchema == "") {
			return resource.Resource{}, fmt.Errorf("%w: target must have status and cannot be an Operation", store.ErrConflict)
		}
		value, err := s.store.Get(ctx, kind, name)
		if err != nil {
			return resource.Resource{}, err
		}
		if value.Metadata.UID != reference["uid"] || value.Metadata.DeletionTimestamp != nil {
			return resource.Resource{}, fmt.Errorf("%w: referenced resource identity changed", store.ErrConflict)
		}
		return value, nil
	}
	return resource.Resource{}, fmt.Errorf("%w: unknown referenced resource kind", store.ErrConflict)
}

func (s *Server) createOperation(ctx context.Context, candidate resource.Resource) (resource.Resource, error) {
	target, err := s.operationResource(ctx, object(candidate.Spec["targetRef"]), true)
	if err != nil {
		return resource.Resource{}, err
	}
	if target.Status["operationRef"] != nil || (target.Kind == "Server" && activeProvisioning(target.Status)) {
		return resource.Resource{}, fmt.Errorf("%w: target is owned by another operation or provisioning run", store.ErrConflict)
	}
	var dependencies []resource.Resource
	if reference := object(candidate.Spec["requestedBy"]); reference != nil {
		requester, readErr := s.operationResource(ctx, reference, false)
		if readErr != nil {
			return resource.Resource{}, readErr
		}
		dependencies = append(dependencies, requester)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	candidate.Status = map[string]any{"phase": "Running", "startedAt": now, "updatedAt": now, "currentStep": "Acquired target"}
	atomic, ok := s.store.(store.AtomicCreator)
	if !ok {
		return resource.Resource{}, fmt.Errorf("atomic operation acquisition is unavailable")
	}
	return atomic.CreateWithStatus(ctx, candidate, target, func(created resource.Resource) map[string]any {
		return applyJSONMergePatch(target.Status, map[string]any{"operationRef": map[string]any{"name": created.Metadata.Name, "uid": created.Metadata.UID}})
	}, dependencies...)
}

func (s *Server) updateOperationStatus(ctx context.Context, current resource.Resource, next map[string]any) (resource.Resource, error) {
	if operationTerminal(current.Status) {
		if resource.EqualJSON(current.Status, next) {
			return current, nil
		}
		return resource.Resource{}, fmt.Errorf("%w: completed Operation status is immutable", store.ErrConflict)
	}
	if next["phase"] == nil || !resource.EqualJSON(current.Status["startedAt"], next["startedAt"]) || next["completedAt"] != nil {
		return resource.Resource{}, fmt.Errorf("%w: phase is required and timestamps are API-owned", store.ErrConflict)
	}
	if current.Status["phase"] == "Uncertain" && next["phase"] == "Running" {
		return resource.Resource{}, fmt.Errorf("%w: uncertain work requires inspected terminal resolution", store.ErrConflict)
	}
	target, err := s.operationResource(ctx, object(current.Spec["targetRef"]), true)
	if err != nil {
		return resource.Resource{}, err
	}
	expected := map[string]any{"name": current.Metadata.Name, "uid": current.Metadata.UID}
	if !resource.EqualJSON(target.Status["operationRef"], expected) {
		return resource.Resource{}, fmt.Errorf("%w: Operation no longer owns this target", store.ErrConflict)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	next["updatedAt"] = now
	current.Status = next
	if !operationTerminal(next) {
		revision, parseErr := requiredRevision(current.Metadata.ResourceVersion)
		if parseErr != nil {
			return resource.Resource{}, parseErr
		}
		return s.store.UpdateStatus(ctx, current.Kind, current.Metadata.Name, next, revision)
	}
	next["completedAt"] = now
	target.Status = applyJSONMergePatch(target.Status, map[string]any{"operationRef": nil})
	atomic, ok := s.store.(store.AtomicStatusUpdater)
	if !ok {
		return resource.Resource{}, fmt.Errorf("atomic operation completion is unavailable")
	}
	return atomic.UpdateStatusPair(ctx, current, target)
}
