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
	APIVersion       string
	PathPrefix       string
	Kind             string
	Plural           string
	CollectionPath   string
	SpecSchema       string
	DecodeSpec       func(json.RawMessage) (any, error)
	DecodeStoredSpec func(map[string]any) (any, error)
}

func NewDefinition[T any](apiVersion, pathPrefix, kind, plural string) Definition {
	decode := func(encoded json.RawMessage) (T, error) {
		var value T
		if err := json.Unmarshal(encoded, &value); err != nil {
			return value, fmt.Errorf("decode %s spec: %w", kind, err)
		}
		return value, nil
	}
	return Definition{
		APIVersion:     apiVersion,
		PathPrefix:     pathPrefix,
		Kind:           kind,
		Plural:         plural,
		CollectionPath: pathPrefix + "/" + plural,
		SpecSchema:     reflect.TypeFor[T]().Name(),
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

func encodeResource[T any](d Definition, apiVersion, kind string, metadata resource.Metadata, typedSpec T, status map[string]any) (resource.Resource, error) {
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

	return resource.Resource{
		APIVersion: d.APIVersion,
		Kind:       d.Kind,
		Metadata:   metadata,
		Spec:       spec,
		Status:     status,
	}, nil
}
