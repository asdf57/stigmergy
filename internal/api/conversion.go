// conversion.go translates between generated API models and generic stored
// resources, including validation of data read back from storage.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
)

type createResourceRequest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   apigen.Metadata `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
}

type resourceEnvelope struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   apigen.Metadata `json:"metadata"`
	Spec       any             `json:"spec"`
}

type resourceListEnvelope struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   apigen.ListMetadata `json:"metadata"`
	Items      []resourceEnvelope  `json:"items"`
}

func (s *Server) apiResource(definition registry.Definition, value resource.Resource) (resourceEnvelope, error) {
	if value.Kind != definition.Kind {
		return resourceEnvelope{}, fmt.Errorf("expected %s, got %s", definition.Kind, value.Kind)
	}
	schemaRef, found := s.openAPI.Components.Schemas[definition.SpecSchema]
	if !found || schemaRef.Value == nil {
		return resourceEnvelope{}, fmt.Errorf("schema %s is not registered", definition.SpecSchema)
	}
	if err := s.openAPI.ValidateSchemaJSON(
		schemaRef.Value,
		value.Spec,
		openapi3.EnableFormatValidation(),
		openapi3.MultiErrors(),
	); err != nil {
		return resourceEnvelope{}, fmt.Errorf("stored %s spec failed validation: %w", definition.Kind, err)
	}
	typedSpec, err := definition.DecodeStoredSpec(value.Spec)
	if err != nil {
		return resourceEnvelope{}, err
	}
	return resourceEnvelope{
		APIVersion: definition.APIVersion,
		Kind:       definition.Kind,
		Metadata:   apiMetadata(value.Metadata),
		Spec:       typedSpec,
	}, nil
}

func decodeJSONBody(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func resourceSpecMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode resource spec: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("convert resource spec: %w", err)
	}
	return result, nil
}

func domainMetadata(value apigen.Metadata) resource.Metadata {
	result := resource.Metadata{Name: value.Name, DeletionTimestamp: value.DeletionTimestamp}
	if value.Uid != nil {
		result.UID = *value.Uid
	}
	if value.ResourceVersion != nil {
		result.ResourceVersion = *value.ResourceVersion
	}
	if value.Generation != nil {
		result.Generation = *value.Generation
	}
	if value.CreationTimestamp != nil {
		result.CreationTimestamp = *value.CreationTimestamp
	}
	if value.Labels != nil {
		result.Labels = *value.Labels
	}
	if value.Annotations != nil {
		result.Annotations = *value.Annotations
	}
	return result
}

func apiMetadata(value resource.Metadata) apigen.Metadata {
	result := apigen.Metadata{Name: value.Name, DeletionTimestamp: value.DeletionTimestamp}
	if value.UID != "" {
		result.Uid = &value.UID
	}
	if value.ResourceVersion != "" {
		result.ResourceVersion = &value.ResourceVersion
	}
	if value.Generation != 0 {
		result.Generation = &value.Generation
	}
	if !value.CreationTimestamp.IsZero() {
		result.CreationTimestamp = &value.CreationTimestamp
	}
	if value.Labels != nil {
		result.Labels = &value.Labels
	}
	if value.Annotations != nil {
		result.Annotations = &value.Annotations
	}
	return result
}

func requiredRevision(value string) (int64, error) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" {
		return 0, errors.New("If-Match with the current resource version is required")
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision <= 0 {
		return 0, errors.New("If-Match must contain a positive resource version")
	}
	return revision, nil
}

func quoteRevision(revision string) string {
	return `"` + revision + `"`
}
