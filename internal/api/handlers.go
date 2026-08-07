// handlers.go implements health checks and the shared create, list, get, put,
// single-delete, and collection-delete behavior used by every resource type.
package api

import (
	"errors"
	"fmt"
	"io"
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
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", "read request body")
		return
	}
	typedSpec, err := definition.DecodeSpec(raw)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	spec, err := resourceSpecMap(typedSpec)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}

	existing, err := s.store.Get(r.Context(), definition.Kind, name)
	if errors.Is(err, store.ErrNotFound) {
		created, createErr := s.store.Create(r.Context(), resource.Resource{
			APIVersion: definition.APIVersion,
			Kind:       definition.Kind,
			Metadata:   resource.Metadata{Name: name},
			Spec:       spec,
		})
		if createErr != nil {
			if errors.Is(createErr, store.ErrConflict) {
				writeError(w, http.StatusConflict, "Conflict", createErr.Error())
				return
			}
			s.writeStoreError(w, createErr)
			return
		}
		body, convertErr := s.apiResource(definition, created)
		if convertErr != nil {
			s.writeStoredResourceError(w, convertErr)
			return
		}
		w.Header().Set("ETag", quoteRevision(created.Metadata.ResourceVersion))
		w.Header().Set("Location", definition.CollectionPath+"/"+created.Metadata.Name)
		writeJSON(w, http.StatusCreated, body)
		return
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	expected, err := strconv.ParseInt(existing.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		s.writeStoredResourceError(w, fmt.Errorf("invalid stored resource version: %w", err))
		return
	}
	existing.Spec = spec
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
