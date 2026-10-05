package api

import (
	"github.com/asdf57/stigmergy/internal/resource"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCommandSpecIsImmutableAcrossPutAndPatch(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			storage := &fakeStore{created: resource.Resource{APIVersion: resource.APIVersion, Kind: "Command", Metadata: resource.Metadata{Name: "run", UID: "command-uid", ResourceVersion: "7", Generation: 1}, Spec: map[string]any{"commandsPipelineRef": map[string]any{"name": "executor"}, "script": "echo original"}}}
			handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
			body := `{"script":"echo changed"}`
			if method == http.MethodPut {
				body = `{"apiVersion":"homelab.io/v1alpha1","kind":"Command","metadata":{"name":"run"},"spec":{"commandsPipelineRef":{"name":"executor"},"script":"echo changed"}}`
			}
			request := httptest.NewRequest(method, "/api/v1alpha1/commands/run", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if method == http.MethodPatch { request.Header.Set("Content-Type", "application/merge-patch+json") }
			request.Header.Set("If-Match", `"7"`)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity || storage.updateCalls != 0 {
				t.Fatalf("immutable update: %d %s writes=%d", response.Code, response.Body, storage.updateCalls)
			}
		})
	}
}
func TestCommandPutIdenticalSpecDoesNotReplayAndAllowsMetadata(t *testing.T) {
	storage := &fakeStore{created: resource.Resource{APIVersion: resource.APIVersion, Kind: "Command", Metadata: resource.Metadata{Name: "run", UID: "command-uid", ResourceVersion: "7", Generation: 1}, Spec: map[string]any{"commandsPipelineRef": map[string]any{"name": "executor"}, "script": "echo original"}, Status: map[string]any{"phase": "Succeeded"}}}
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body := `{"apiVersion":"homelab.io/v1alpha1","kind":"Command","metadata":{"name":"run","labels":{"purpose":"example"}},"spec":{"commandsPipelineRef":{"name":"executor"},"script":"echo original"}}`
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPut, "/api/v1alpha1/commands/run", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%d %s", response.Code, response.Body)
		}
	}
	if storage.updateCalls != 1 || storage.created.Metadata.Generation != 1 || storage.created.Status["phase"] != "Succeeded" {
		t.Fatal("metadata/no-op update changed execution", storage.created)
	}
}
