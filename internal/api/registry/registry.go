// Package registry converts generated spec types into runtime resource
// definitions used by the generic API router and storage handlers.
package registry

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/asdf57/prov-controller-test/go/internal/resource"
)

type Definition struct {
	APIVersion        string
	PathPrefix        string
	Kind              string
	Plural            string
	CollectionPath    string
	SpecSchema        string
	StatusSchema      string
	DefaultFinalizers []string
	DecodeSpec        func(json.RawMessage) (any, error)
	DecodeStoredSpec  func(map[string]any) (any, error)
}

func NewDefinition[T any](apiVersion, pathPrefix, kind, plural, statusSchema string, defaultFinalizers []string) Definition {
	decode := func(encoded json.RawMessage) (T, error) {
		var value T
		if err := json.Unmarshal(encoded, &value); err != nil {
			return value, fmt.Errorf("decode %s spec: %w", kind, err)
		}
		return value, nil
	}
	return Definition{
		APIVersion:        apiVersion,
		PathPrefix:        pathPrefix,
		Kind:              kind,
		Plural:            plural,
		CollectionPath:    pathPrefix + "/" + plural,
		SpecSchema:        reflect.TypeFor[T]().Name(),
		StatusSchema:      statusSchema,
		DefaultFinalizers: append([]string(nil), defaultFinalizers...),
		DecodeSpec: func(encoded json.RawMessage) (any, error) {
			return decode(encoded)
		},
		DecodeStoredSpec: func(stored map[string]any) (any, error) {
			return decodeStoredSpec[T](kind, stored)
		},
	}
}

func decodeResourceSpec[T any](d Definition, value resource.Resource) (T, error) {
	var zero T
	if value.APIVersion != d.APIVersion {
		return zero, fmt.Errorf("expected apiVersion %q, got %q", d.APIVersion, value.APIVersion)
	}
	if value.Kind != d.Kind {
		return zero, fmt.Errorf("expected kind %q, got %q", d.Kind, value.Kind)
	}
	return decodeStoredSpec[T](d.Kind, value.Spec)
}

func decodeStoredSpec[T any](kind string, stored map[string]any) (T, error) {
	var zero T
	encoded, err := json.Marshal(stored)
	if err != nil {
		return zero, fmt.Errorf("encode stored %s spec: %w", kind, err)
	}
	var spec T
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return zero, fmt.Errorf("decode %s spec: %w", kind, err)
	}
	return spec, nil
}

func decodeStoredStatus[T any](kind string, stored map[string]any) (*T, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode stored %s status: %w", kind, err)
	}
	var status T
	if err := json.Unmarshal(encoded, &status); err != nil {
		return nil, fmt.Errorf("decode %s status: %w", kind, err)
	}
	return &status, nil
}

func encodeStatus[T any](kind string, status *T) (map[string]any, error) {
	encoded, err := json.Marshal(status)
	if err != nil {
		return nil, fmt.Errorf("encode %s status: %w", kind, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return nil, fmt.Errorf("convert %s status for storage: %w", kind, err)
	}
	return stored, nil
}

func encodeResource[T any](d Definition, apiVersion, kind string, metadata resource.Metadata, typedSpec T, typedStatus any) (resource.Resource, error) {
	if apiVersion != "" && apiVersion != d.APIVersion {
		return resource.Resource{}, fmt.Errorf("expected apiVersion %q, got %q", d.APIVersion, apiVersion)
	}
	if kind != "" && kind != d.Kind {
		return resource.Resource{}, fmt.Errorf("expected kind %q, got %q", d.Kind, kind)
	}

	encoded, err := json.Marshal(typedSpec)
	if err != nil {
		return resource.Resource{}, fmt.Errorf("encode %s spec: %w", d.Kind, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return resource.Resource{}, fmt.Errorf("convert %s spec for storage: %w", d.Kind, err)
	}
	var status map[string]any
	if typedStatus != nil {
		encoded, err := json.Marshal(typedStatus)
		if err != nil {
			return resource.Resource{}, fmt.Errorf("encode %s status: %w", d.Kind, err)
		}
		if err := json.Unmarshal(encoded, &status); err != nil {
			return resource.Resource{}, fmt.Errorf("convert %s status for storage: %w", d.Kind, err)
		}
	}

	return resource.Resource{
		APIVersion: d.APIVersion,
		Kind:       d.Kind,
		Metadata:   metadata,
		Spec:       spec,
		Status:     status,
	}, nil
}
