// Package api assembles the HTTP API, validation middleware, resource registry,
// and storage-backed handlers into the control plane's HTTP server.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

const maxRequestBody = 1 << 20

type Server struct {
	logger         *slog.Logger
	store          store.Store
	requestTimeout time.Duration
	openAPI        *openapi3.T
	resources      map[string]registry.Definition
}

func New(logger *slog.Logger, resourceStore store.Store, requestTimeout time.Duration) http.Handler {
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
	for _, definition := range registry.Definitions {
		server.resources[definition.CollectionPath] = definition
	}

	validate := nethttpmiddleware.OapiRequestValidatorWithOptions(specification, &nethttpmiddleware.Options{
		DoNotValidateServers: true,
		ErrorHandler:         server.handleValidationError,
	})

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /healthz", server.getLiveness)
	apiMux.HandleFunc("GET /readyz", server.getReadiness)
	apiMux.HandleFunc("/", server.serveResource)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.json", serveOpenAPI)
	mux.Handle("GET /docs/", swaggerHandler())
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusTemporaryRedirect)
	})
	mux.Handle("/", validate(apiMux))

	return server.recover(server.withTimeout(server.limitRequestBody(mux)))
}
