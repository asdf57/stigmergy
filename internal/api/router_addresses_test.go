package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
)

func TestRouterAddressSchema(t *testing.T) {
	const base = `{"apiVersion":"homelab.io/v1alpha1","kind":"Router","metadata":{"name":"router"},"spec":{"authentication":{"type":"UsernamePasswordCredential","name":"credentials"},"mgmtAddr":"10.0.2.1","type":"Mikrotik"`
	tests := []struct {
		name      string
		addresses string
		status    int
	}{
		{"omitted", "", http.StatusCreated},
		{"explicit empty", `,"addresses":[]`, http.StatusCreated},
		{"named address", `,"addresses":[{"name":"metallb","address":"10.0.3.1/24","network":"10.0.3.0","interface":"bridge"}]`, http.StatusCreated},
		{"point-to-point", `,"addresses":[{"name":"management","address":"10.0.2.1/32","network":"10.0.2.0","interface":"bridge"}]`, http.StatusCreated},
		{"missing identity", `,"addresses":[{"address":"10.0.3.1/24","network":"10.0.3.0","interface":"bridge"}]`, http.StatusBadRequest},
		{"invalid name", `,"addresses":[{"name":"bad:name","address":"10.0.3.1/24","network":"10.0.3.0","interface":"bridge"}]`, http.StatusBadRequest},
		{"read-only interface", `,"addresses":[{"name":"metallb","address":"10.0.3.1/24","network":"10.0.3.0","interface":"bridge","actualInterface":"bridge"}]`, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeStore{}, time.Second)
			request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/routers", strings.NewReader(base+test.addresses+`}}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if response.Code != http.StatusCreated {
				return
			}
			var router apigen.Router
			if err := json.Unmarshal(response.Body.Bytes(), &router); err != nil {
				t.Fatal(err)
			}
			if test.name == "omitted" && router.Spec.Addresses != nil {
				t.Fatal("omitted addresses became managed")
			}
			if test.name == "explicit empty" && (router.Spec.Addresses == nil || len(*router.Spec.Addresses) != 0) {
				t.Fatal("explicit empty list lost its cleanup intent")
			}
		})
	}
}
