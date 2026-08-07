// response.go provides the common JSON success and structured error writers.
package api

import (
	"encoding/json"
	"net/http"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse(code, message))
}

func errorResponse(code, message string) apigen.Error {
	var response apigen.Error
	response.Error.Code = code
	response.Error.Message = message
	return response
}
