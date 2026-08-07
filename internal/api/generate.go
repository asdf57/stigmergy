// generate.go defines the reproducible pipeline that builds the resource
// registry, bundled OpenAPI document, and generated Go API models.
package api

//go:generate go run ./cmd/generate-api -base spec/openapi.yaml -resources spec/resources -spec-output spec/openapi.bundle.yaml -registry-output registry/registry.gen.go -models-import github.com/asdf57/prov-controller-test/go/internal/api/gen
//go:generate go tool oapi-codegen -config gen/oapi-codegen.yaml spec/openapi.bundle.yaml
