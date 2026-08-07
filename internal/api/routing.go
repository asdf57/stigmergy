// routing.go resolves concrete OpenAPI resource paths to generated resource
// registrations and dispatches collection or item requests to shared handlers.
package api

import (
	"net/http"
	"strings"

	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
)

func (s *Server) serveResource(w http.ResponseWriter, r *http.Request) {
	definition, resourceName, collection, found := s.resolveResource(r.URL.Path)
	if !found {
		writeError(w, http.StatusNotFound, "NotFound", "resource endpoint not found")
		return
	}

	if collection {
		switch r.Method {
		case http.MethodGet:
			s.listResources(w, r, definition)
		case http.MethodPost:
			s.createResource(w, r, definition)
		case http.MethodDelete:
			s.deleteResources(w, r, definition)
		default:
			writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method is not supported")
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getResource(w, r, definition, resourceName)
	case http.MethodPut:
		s.putResource(w, r, definition, resourceName)
	case http.MethodDelete:
		s.deleteResource(w, r, definition, resourceName)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method is not supported")
	}
}

// Resolve relative URL path into internal API object (e.g. /api/v1alpha1/machine-reports)
// Includes both generic and explicit objects
// Returns: (resource definition, individual resource path, isCollection, isFound)
func (s *Server) resolveResource(path string) (registry.Definition, string, bool, bool) {
	// If the path directly corresponds to a resource type, return it
	if definition, found := s.resources[path]; found {
		return definition, "", true, true
	}

	separator := strings.LastIndexByte(path, '/')

	// If we find no path or if we provide an empty resource name, return immediately
	if separator < 0 || separator == len(path)-1 {
		return registry.Definition{}, "", false, false
	}

	// Grab the resource type
	definition, found := s.resources[path[:separator]]
	if !found {
		return registry.Definition{}, "", false, false
	}

	// Return the resource type and resolved resource name
	return definition, path[separator+1:], false, true
}
