// Package api assembles the HTTP API, validation middleware, resource registry,
// and storage-backed handlers into the control plane's HTTP server.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/store"
)

const maxRequestBody = 1 << 20

type Server struct {
	logger         *slog.Logger
	store          store.Store
	requestTimeout time.Duration
	openAPI        *openapi3.T
	resources      map[string]registry.Definition
	discoveryISO   string
}

func New(logger *slog.Logger, resourceStore store.Store, requestTimeout time.Duration, discoveryISO ...string) http.Handler {
	specification, err := apigen.GetSpec()
	if err != nil {
		panic(fmt.Errorf("load embedded OpenAPI specification: %w", err))
	}
	server := &Server{
		logger:         logger,
		store:          resourceStore,
		requestTimeout: requestTimeout,
		openAPI:        specification,
		resources:      make(map[string]registry.Definition, len(registry.Definitions)),
	}
	if len(discoveryISO) > 0 {
		server.discoveryISO = discoveryISO[0]
	}
	for _, definition := range registry.Definitions {
		server.resources[definition.CollectionPath] = definition
	}

	validate := nethttpmiddleware.OapiRequestValidatorWithOptions(specification, &nethttpmiddleware.Options{
		// The outer WithAccessPolicy middleware authenticates and authorizes
		// before validation. Use the library's standard hook to avoid a second
		// auth check; main rejects missing policy unless explicit dev mode is set.
		Options:              openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		DoNotValidateServers: true,
		ErrorHandler:         server.handleValidationError,
	})

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /healthz", server.getLiveness)
	apiMux.HandleFunc("GET /readyz", server.getReadiness)
	apiMux.HandleFunc("DELETE /api/v1alpha1/resources", server.deleteAllResources)
	apiMux.HandleFunc("/", server.serveResource)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.json", serveOpenAPI)
	mux.Handle("GET /docs/", swaggerHandler())
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /ipxe/{mac}", server.getIPXEBoot)
	mux.Handle("/", server.normalizeYAML(validate(apiMux)))

	return server.recover(server.withTimeout(server.limitRequestBody(mux)))
}
