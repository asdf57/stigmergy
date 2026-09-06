// handlers.go implements health checks and the shared create, list, get, put,
// single-delete, and collection-delete behavior used by every resource type.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

func (s *Server) getLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, apigen.Health{Status: "ok"})
}

func (s *Server) getReadiness(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ready(r.Context()); err != nil {
		s.logger.Warn("readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "NotReady", "resource store is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, apigen.Health{Status: "ready"})
}

func (s *Server) deleteAllResources(w http.ResponseWriter, r *http.Request) {
	for _, definition := range registry.Definitions {
		deleted, err := s.store.DeleteCollection(r.Context(), definition.Kind)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		s.logger.Info("deleted resources", "kind", definition.Kind, "count", deleted)
	}
	writeJSON(w, http.StatusOK, apigen.DeleteCollectionResult{Deleted: -1})
}

func (s *Server) createResource(w http.ResponseWriter, r *http.Request, definition registry.Definition) {
	var request createResourceRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	if request.APIVersion != definition.APIVersion || request.Kind != definition.Kind {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", "apiVersion and kind do not match the resource endpoint")
		return
	}
	typedSpec, err := definition.DecodeSpec(request.Spec)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	spec, err := resourceSpecMap(typedSpec)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	candidate := resource.Resource{
		APIVersion: definition.APIVersion,
		Kind:       definition.Kind,
		Metadata:   domainMetadata(request.Metadata),
		Spec:       spec,
	}
	if err := resource.ValidateCreate(candidate); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	candidate.Metadata.Finalizers = append([]string(nil), definition.DefaultFinalizers...)

	created, err := s.store.Create(r.Context(), candidate)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
		s.writeStoreError(w, err)
		return
	}
	body, err := s.apiResource(definition, created)
	if err != nil {
		s.writeStoredResourceError(w, err)
		return
	}
	w.Header().Set("ETag", quoteRevision(created.Metadata.ResourceVersion))
	w.Header().Set("Location", definition.CollectionPath+"/"+created.Metadata.Name)
	writeJSON(w, http.StatusCreated, body)
}

func (s *Server) listResources(w http.ResponseWriter, r *http.Request, definition registry.Definition) {
	resources, err := s.store.List(r.Context(), definition.Kind)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	items := make([]resourceEnvelope, len(resources.Items))
	for index := range resources.Items {
		items[index], err = s.apiResource(definition, resources.Items[index])
		if err != nil {
			s.writeStoredResourceError(w, err)
			return
		}
	}
	revision := resources.Metadata.ResourceVersion
	w.Header().Set("X-Resource-Version", revision)
	writeJSON(w, http.StatusOK, resourceListEnvelope{
		APIVersion: definition.APIVersion,
		Kind:       definition.Kind + "List",
		Metadata:   apigen.ListMetadata{ResourceVersion: revision},
		Items:      items,
	})
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request, definition registry.Definition, name string) {
	result, err := s.store.Get(r.Context(), definition.Kind, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NotFound", err.Error())
			return
		}
		s.writeStoreError(w, err)
		return
	}
	body, err := s.apiResource(definition, result)
	if err != nil {
		s.writeStoredResourceError(w, err)
		return
	}
	w.Header().Set("ETag", quoteRevision(result.Metadata.ResourceVersion))
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) putResource(w http.ResponseWriter, r *http.Request, definition registry.Definition, name string) {
	var request createResourceRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	if request.APIVersion != definition.APIVersion || request.Kind != definition.Kind {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", "apiVersion and kind do not match the resource endpoint")
		return
	}
	if request.Metadata.Name != name {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", "metadata.name does not match the resource URL")
		return
	}
	typedSpec, err := definition.DecodeSpec(request.Spec)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	spec, err := resourceSpecMap(typedSpec)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}

	candidate := resource.Resource{
		APIVersion: definition.APIVersion,
		Kind:       definition.Kind,
		Metadata:   domainMetadata(request.Metadata),
		Spec:       spec,
	}
	if err := resource.ValidateCreate(candidate); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	expected, conditional, err := optionalRevision(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid", err.Error())
		return
	}

	const maxAttempts = 3
	var retryBase *resource.Resource
	for attempt := 0; attempt < maxAttempts; attempt++ {
		existing, getErr := s.store.Get(r.Context(), definition.Kind, name)
		if errors.Is(getErr, store.ErrNotFound) {
			if conditional {
				writeError(w, http.StatusConflict, "Conflict", store.ErrConflict.Error())
				return
			}
			candidate.Metadata.Finalizers = append([]string(nil), definition.DefaultFinalizers...)
			created, createErr := s.store.Create(r.Context(), candidate)
			if errors.Is(createErr, store.ErrConflict) {
				base := candidate
				retryBase = &base
				continue
			}
			if createErr != nil {
				s.writeStoreError(w, createErr)
				return
			}
			s.writePutResource(w, definition, created, http.StatusCreated)
			return
		}
		if getErr != nil {
			s.writeStoreError(w, getErr)
			return
		}
		if retryBase != nil && !sameClientOwnedState(existing, *retryBase) && !sameClientOwnedState(existing, candidate) {
			writeError(w, http.StatusConflict, "Conflict", "client-owned resource state changed concurrently")
			return
		}
		if conditional && existing.Metadata.ResourceVersion != strconv.FormatInt(expected, 10) {
			writeError(w, http.StatusConflict, "Conflict", store.ErrConflict.Error())
			return
		}
		if sameClientOwnedState(existing, candidate) {
			s.writePutResource(w, definition, existing, http.StatusOK)
			return
		}
		if !conditional {
			expected, err = strconv.ParseInt(existing.Metadata.ResourceVersion, 10, 64)
			if err != nil {
				s.writeStoredResourceError(w, fmt.Errorf("invalid stored resource version: %w", err))
				return
			}
		}
		observed := existing
		existing.Spec = candidate.Spec
		existing.Metadata.Labels = candidate.Metadata.Labels
		existing.Metadata.Annotations = candidate.Metadata.Annotations
		updated, updateErr := s.store.Update(r.Context(), existing, expected)
		if errors.Is(updateErr, store.ErrConflict) || errors.Is(updateErr, store.ErrNotFound) {
			if conditional {
				writeError(w, http.StatusConflict, "Conflict", updateErr.Error())
				return
			}
			retryBase = &observed
			continue
		}
		if updateErr != nil {
			s.writeStoreError(w, updateErr)
			return
		}
		s.writePutResource(w, definition, updated, http.StatusOK)
		return
	}
	writeError(w, http.StatusConflict, "Conflict", "resource changed during PUT retries")
}

func sameClientOwnedState(left, right resource.Resource) bool {
	return resource.EqualJSON(left.Spec, right.Spec) &&
		resource.EqualJSON(left.Metadata.Labels, right.Metadata.Labels) &&
		resource.EqualJSON(left.Metadata.Annotations, right.Metadata.Annotations)
}

func (s *Server) writePutResource(w http.ResponseWriter, definition registry.Definition, value resource.Resource, status int) {
	body, err := s.apiResource(definition, value)
	if err != nil {
		s.writeStoredResourceError(w, err)
		return
	}
	w.Header().Set("ETag", quoteRevision(value.Metadata.ResourceVersion))
	if status == http.StatusCreated {
		w.Header().Set("Location", definition.CollectionPath+"/"+value.Metadata.Name)
	}
	writeJSON(w, status, body)
}

func (s *Server) patchResource(w http.ResponseWriter, r *http.Request, definition registry.Definition, name string) {
	expected, err := requiredRevision(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "PreconditionRequired", err.Error())
		return
	}
	var patch map[string]any
	if err := decodeJSONBody(r, &patch); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}

	existing, err := s.store.Get(r.Context(), definition.Kind, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NotFound", err.Error())
			return
		}
		s.writeStoreError(w, err)
		return
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(expected, 10) {
		writeError(w, http.StatusConflict, "Conflict", store.ErrConflict.Error())
		return
	}

	merged := applyJSONMergePatch(existing.Spec, patch)
	validated, err := s.validateResourceSpec(definition, merged)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	existing.Spec = validated
	updated, err := s.store.Update(r.Context(), existing, expected)
	if err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
		s.writeStoreError(w, err)
		return
	}
	body, err := s.apiResource(definition, updated)
	if err != nil {
		s.writeStoredResourceError(w, err)
		return
	}
	w.Header().Set("ETag", quoteRevision(updated.Metadata.ResourceVersion))
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) deleteResource(w http.ResponseWriter, r *http.Request, definition registry.Definition, name string) {
	expected, err := requiredRevision(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "PreconditionRequired", err.Error())
		return
	}
	existing, err := s.store.Get(r.Context(), definition.Kind, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "NotFound", err.Error())
			return
		}
		s.writeStoreError(w, err)
		return
	}
	if existing.Metadata.ResourceVersion != strconv.FormatInt(expected, 10) {
		writeError(w, http.StatusConflict, "Conflict", store.ErrConflict.Error())
		return
	}
	waitsForFinalizers := len(existing.Metadata.Finalizers) != 0
	if err := s.store.Delete(r.Context(), definition.Kind, name, expected); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "NotFound", err.Error())
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, "Conflict", err.Error())
		default:
			s.writeStoreError(w, err)
		}
		return
	}
	if waitsForFinalizers {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteResources(w http.ResponseWriter, r *http.Request, definition registry.Definition) {
	deleted, err := s.store.DeleteCollection(r.Context(), definition.Kind)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.DeleteCollectionResult{Deleted: deleted})
}
