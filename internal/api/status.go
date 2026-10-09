package api

import (
	"github.com/getkin/kin-openapi/openapi3"
	"net/http"
	"strconv"

	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
)

// Status merges preserve other writers' fields. The UID and ETag bind the
// observation to the resource lifetime and snapshot the caller actually verified.
func (s *Server) patchResourceStatus(w http.ResponseWriter, r *http.Request, definition registry.Definition, name string) {
	expected, err := requiredRevision(r.Header.Get("If-Match"))
	if err != nil {
		writeError(w, http.StatusPreconditionRequired, "PreconditionRequired", err.Error())
		return
	}
	var patch struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Status map[string]any `json:"status"`
	}
	if err := decodeJSONBody(r, &patch); err != nil || patch.Metadata.UID == "" || patch.Status == nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", "status patch requires metadata.uid and a status object; spec/other metadata are forbidden")
		return
	}
	current, err := s.store.Get(r.Context(), definition.Kind, name)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if current.Metadata.DeletionTimestamp != nil || current.Metadata.UID != patch.Metadata.UID || current.Metadata.ResourceVersion != strconv.FormatInt(expected, 10) {
		writeError(w, http.StatusConflict, "Conflict", "resource identity or version changed; fetch and verify again")
		return
	}
	merged := applyJSONMergePatch(current.Status, patch.Status)
	schema := s.openAPI.Components.Schemas[definition.StatusSchema]
	if schema == nil || schema.Value == nil {
		writeError(w, http.StatusInternalServerError, "Internal", "status schema is unavailable")
		return
	}
	if err := s.openAPI.ValidateSchemaJSON(schema.Value, merged, openapi3.EnableFormatValidation(), openapi3.MultiErrors()); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "Invalid", err.Error())
		return
	}
	if definition.Kind == registry.ServerResource.Kind {
		if err := validateServerOperation(current, merged); err != nil {
			writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
		if err := s.validateProvisioningStatus(r.Context(), current, merged); err != nil {
			writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
		if _, present := patch.Status["installedSSHTrustBundleDigest"]; present {
			if err := s.validateServerTrustStatus(r.Context(), current, merged); err != nil {
				writeError(w, http.StatusConflict, "Conflict", err.Error())
				return
			}
		}
	}
	if definition.Kind == "ProvisioningRun" {
		if err := s.validateRunReservation(r.Context(), current, merged); err != nil {
			writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
		if err := validateProvisioningRunStatus(current, merged); err != nil {
			writeError(w, http.StatusConflict, "Conflict", err.Error())
			return
		}
	}
	updated := current
	if !resource.EqualJSON(current.Status, merged) {
		updated, err = s.store.UpdateStatus(r.Context(), current.Kind, current.Metadata.Name, merged, expected)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	body, err := s.apiResource(definition, updated)
	if err != nil {
		s.writeStoredResourceError(w, err)
		return
	}
	w.Header().Set("ETag", quoteRevision(updated.Metadata.ResourceVersion))
	writeJSON(w, http.StatusOK, body)
}
