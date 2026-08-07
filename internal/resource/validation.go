package resource

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	maxMetadataEntries = 32
	maxMetadataValue   = 4096
)

var (
	kindPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$`)
)

func Validate(resource Resource) error {
	var problems []error

	if resource.APIVersion != APIVersion {
		problems = append(problems, fmt.Errorf("apiVersion must be %q", APIVersion))
	}
	if !ValidKind(resource.Kind) {
		problems = append(problems, errors.New("kind must start with an uppercase letter and contain only letters or digits"))
	}
	if !ValidName(resource.Metadata.Name) {
		problems = append(problems, errors.New("metadata.name must be a lowercase DNS-style name"))
	}
	if resource.Spec == nil {
		problems = append(problems, errors.New("spec must be a JSON object"))
	}
	if len(resource.Metadata.Finalizers) != 0 {
		problems = append(problems, errors.New("finalizers are not supported until finalization reconciliation is implemented"))
	}
	if err := validateMap("metadata.labels", resource.Metadata.Labels); err != nil {
		problems = append(problems, err)
	}
	if err := validateMap("metadata.annotations", resource.Metadata.Annotations); err != nil {
		problems = append(problems, err)
	}

	return errors.Join(problems...)
}

func ValidateCreate(resource Resource) error {
	var problems []error
	if err := Validate(resource); err != nil {
		problems = append(problems, err)
	}
	if resource.Metadata.UID != "" || resource.Metadata.ResourceVersion != "" || resource.Metadata.Generation != 0 || !resource.Metadata.CreationTimestamp.IsZero() {
		problems = append(problems, errors.New("uid, resourceVersion, generation, and creationTimestamp are server-owned"))
	}
	if resource.Metadata.DeletionTimestamp != nil {
		problems = append(problems, errors.New("deletionTimestamp is server-owned"))
	}
	if len(resource.Status) != 0 {
		problems = append(problems, errors.New("status cannot be set during creation"))
	}
	return errors.Join(problems...)
}

func ValidKind(kind string) bool {
	return kindPattern.MatchString(kind)
}

func ValidName(name string) bool {
	return len(name) <= 253 && namePattern.MatchString(name)
}

func validateMap(field string, values map[string]string) error {
	if len(values) > maxMetadataEntries {
		return fmt.Errorf("%s cannot contain more than %d entries", field, maxMetadataEntries)
	}
	for key, value := range values {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s contains an empty key", field)
		}
		if len(key) > maxMetadataValue || len(value) > maxMetadataValue {
			return fmt.Errorf("%s keys and values cannot exceed %d bytes", field, maxMetadataValue)
		}
	}
	return nil
}
