// Package registry converts generated spec types into runtime resource
// definitions used by the generic API router and storage handlers.
package registry

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	decode := func(encoded json.RawMessage) (any, error) {
		var value T
		if err := json.Unmarshal(encoded, &value); err != nil {
			return nil, fmt.Errorf("decode %s spec: %w", kind, err)
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
		DecodeSpec:     decode,
		DecodeStoredSpec: func(stored map[string]any) (any, error) {
			encoded, err := json.Marshal(stored)
			if err != nil {
				return nil, fmt.Errorf("encode stored %s spec: %w", kind, err)
			}
			return decode(json.RawMessage(encoded))
		},
	}
}
