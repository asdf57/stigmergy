// middleware.go contains API-wide request limits, timeouts, panic recovery,
// validation error handling, and safe internal-error responses.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"go.yaml.in/yaml/v3"
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

func (s *Server) normalizeYAML(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		targetMediaType, supported := map[string]string{
			"application/yaml":             "application/json",
			"application/x-yaml":           "application/json",
			"application/merge-patch+yaml": "application/merge-patch+json",
		}[mediaType]
		if !supported {
			next.ServeHTTP(w, r)
			return
		}

		decoder := yaml.NewDecoder(r.Body)
		var document any
		if err := decoder.Decode(&document); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid", "decode YAML request body: "+err.Error())
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			if err == nil {
				err = errors.New("multiple YAML documents are not supported")
			}
			writeError(w, http.StatusBadRequest, "Invalid", "decode YAML request body: "+err.Error())
			return
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid", "convert YAML request body: "+err.Error())
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(encoded))
		r.ContentLength = int64(len(encoded))
		r.Header.Set("Content-Type", targetMediaType)
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
