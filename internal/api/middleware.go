// middleware.go contains API-wide request limits, timeouts, panic recovery,
// validation error handling, and safe internal-error responses.
package api

import (
	"context"
	"net/http"
	"strings"
)

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	s.logger.Error("resource store request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "Internal", "resource store request failed")
}

func (s *Server) writeStoredResourceError(w http.ResponseWriter, err error) {
	s.logger.Error("stored resource is invalid", "error", err)
	writeError(w, http.StatusInternalServerError, "Internal", "stored resource is invalid")
}

func (s *Server) handleValidationError(w http.ResponseWriter, message string, status int) {
	if strings.Contains(message, "If-Match") && strings.Contains(strings.ToLower(message), "required") {
		writeError(w, http.StatusPreconditionRequired, "PreconditionRequired", "If-Match with the current resource version is required")
		return
	}
	writeError(w, status, "Invalid", message)
}

func (s *Server) limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic serving request", "panic", recovered, "method", r.Method, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "Internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
