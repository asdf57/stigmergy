// openapi.go serves the generated OpenAPI document and embedded Swagger UI.
package api

import (
	"net/http"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/swaggest/swgui"
	"github.com/swaggest/swgui/v5emb"
)

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	document, err := apigen.GetSpecJSON()
	if err != nil {
		http.Error(w, "openapi document unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document)
}

func swaggerHandler() http.Handler {
	return v5emb.NewWithConfig(swgui.Config{
		SettingsUI: map[string]string{
			"deepLinking":            "true",
			"displayRequestDuration": "true",
			"persistAuthorization":   "true",
			"validatorUrl":           "null",
		},
	})("Homelab Control Plane API", "/openapi.json", "/docs/")
}
