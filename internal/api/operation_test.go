package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/testutil"
)

func operationFixture(kind string) (resource.Resource, resource.Resource) {
	target := resource.Resource{APIVersion: resource.APIVersion, Kind: kind, Metadata: resource.Metadata{Name: "target", UID: "target-uid", Generation: 1, ResourceVersion: "1"}, Spec: map[string]any{}, Status: map[string]any{}}
	operation := resource.Resource{APIVersion: resource.APIVersion, Kind: "Operation", Metadata: resource.Metadata{Name: "work"}, Spec: map[string]any{
		"targetRef": map[string]any{"kind": kind, "name": "target", "uid": "target-uid"}, "action": "configure", "operator": "test-operator",
	}}
	return target, operation
}

func TestOperationGenericOwnershipAndCompletion(t *testing.T) {
	for _, kind := range []string{"Server", "KubeCluster", "ISO"} {
		t.Run(kind, func(t *testing.T) {
			target, candidate := operationFixture(kind)
			storage := testutil.NewStore(target)
			api := &Server{store: storage}
			created, err := api.createOperation(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			owner, _ := storage.Get(context.Background(), kind, "target")
			if object(owner.Status["operationRef"])["uid"] != created.Metadata.UID {
				t.Fatal("missing target ownership")
			}
			candidate.Metadata.Name = "second"
			if _, err := api.createOperation(context.Background(), candidate); err == nil {
				t.Fatal("overlapping ownership accepted")
			}
			if err := storage.Delete(context.Background(), kind, "target", 2); err == nil {
				t.Fatal("active target deleted")
			}
			uncertain := applyJSONMergePatch(created.Status, map[string]any{"phase": "Uncertain", "message": "Inspect remote work"})
			created, err = api.updateOperationStatus(context.Background(), created, uncertain)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := api.updateOperationStatus(context.Background(), created, applyJSONMergePatch(created.Status, map[string]any{"phase": "Running"})); err == nil {
				t.Fatal("uncertain work resumed")
			}
			created, err = api.updateOperationStatus(context.Background(), created, applyJSONMergePatch(created.Status, map[string]any{"phase": "Failed"}))
			if err != nil {
				t.Fatal(err)
			}
			owner, _ = storage.Get(context.Background(), kind, "target")
			if owner.Status["operationRef"] != nil || created.Status["completedAt"] == nil {
				t.Fatal("terminal completion did not release target atomically")
			}
			if _, err := api.createOperation(context.Background(), candidate); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOperationHTTPStatusAndIdentityGuards(t *testing.T) {
	target, candidate := operationFixture("KubeCluster")
	storage := testutil.NewStore(target)
	handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
	body, _ := json.Marshal(map[string]any{"apiVersion": candidate.APIVersion, "kind": candidate.Kind, "metadata": map[string]any{"name": candidate.Metadata.Name}, "spec": candidate.Spec})
	request := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/operations", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	created, _ := storage.Get(context.Background(), "Operation", "work")
	for _, path := range []string{"/api/v1alpha1/operations/work", "/api/v1alpha1/kube-clusters/target"} {
		kind, name := "Operation", "work"
		if strings.Contains(path, "kube-clusters") {
			kind, name = "KubeCluster", "target"
		}
		value, _ := storage.Get(context.Background(), kind, name)
		request := httptest.NewRequest(http.MethodDelete, path, nil)
		request.Header.Set("If-Match", `"`+value.Metadata.ResourceVersion+`"`)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("active deletion allowed: %d %s", response.Code, response.Body.String())
		}
	}
	owner, _ := storage.Get(context.Background(), "KubeCluster", "target")
	forged := httptest.NewRequest(http.MethodPatch, "/api/v1alpha1/kube-clusters/target/status", strings.NewReader(`{"metadata":{"uid":"target-uid"},"status":{"operationRef":null}}`))
	forged.Header.Set("Content-Type", "application/merge-patch+json")
	forged.Header.Set("If-Match", `"`+owner.Metadata.ResourceVersion+`"`)
	forgedReply := httptest.NewRecorder()
	handler.ServeHTTP(forgedReply, forged)
	if forgedReply.Code != http.StatusConflict {
		t.Fatal("ordinary status write released ownership")
	}
	patch := func(uid, revision, phase string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPatch, "/api/v1alpha1/operations/work/status", strings.NewReader(fmt.Sprintf(`{"metadata":{"uid":%q},"status":{"phase":%q}}`, uid, phase)))
		request.Header.Set("Content-Type", "application/merge-patch+json")
		request.Header.Set("If-Match", `"`+revision+`"`)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for _, result := range []*httptest.ResponseRecorder{patch("replacement", created.Metadata.ResourceVersion, "Succeeded"), patch(created.Metadata.UID, "999", "Succeeded")} {
		if result.Code != http.StatusConflict {
			t.Fatalf("identity fence: %d %s", result.Code, result.Body.String())
		}
	}
	result := patch(created.Metadata.UID, created.Metadata.ResourceVersion, "Succeeded")
	if result.Code != http.StatusOK {
		t.Fatalf("finish: %d %s", result.Code, result.Body.String())
	}
	owner, _ = storage.Get(context.Background(), "KubeCluster", "target")
	if owner.Status["operationRef"] != nil {
		t.Fatal("target retained after completion")
	}
}

func TestOperationAndProvisioningExcludeEachOther(t *testing.T) {
	host, machine, run := runFixture()
	_, candidate := operationFixture("Server")
	candidate.Spec["targetRef"] = map[string]any{"kind": "Server", "name": host.Metadata.Name, "uid": host.Metadata.UID}
	for _, operationFirst := range []bool{true, false} {
		api := &Server{store: newRunStore(host, machine)}
		if operationFirst {
			if _, err := api.createOperation(context.Background(), candidate); err != nil {
				t.Fatal(err)
			}
			if _, err := api.createProvisioningRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "Operation work") {
				t.Fatalf("missing busy reason: %v", err)
			}
		} else {
			if _, err := api.createProvisioningRun(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			if _, err := api.createOperation(context.Background(), candidate); err == nil {
				t.Fatal("claimed provisioning-owned Server")
			}
		}
	}
}
