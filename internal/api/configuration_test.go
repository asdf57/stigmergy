package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfigurationAndClusterGeneratedRoutes(t *testing.T) {
	configuration := `{"apiVersion":"homelab.io/v1alpha1","kind":"Configuration","metadata":{"name":"base"},"spec":{"targetRef":{"kind":"Server","name":"node"},"inventoryCaptureGroupRef":{"name":"managed"},"source":{"repositoryRef":{"name":"roles"}},"command":{"argv":["ansible-playbook","plays/base.yml"]}}}`
	cluster := `{"apiVersion":"homelab.io/v1alpha1","kind":"KubeCluster","metadata":{"name":"lab"},"spec":{"kubernetesVersion":"1.34.1","containerdVersion":"2.1.5","controlPlaneEndpoint":"lab.example.test:6443","podCIDR":"10.244.0.0/16","serviceCIDR":"10.96.0.0/12","network":{"provider":"cilium","version":"1.18.1"}}}`
	for _, test := range []struct {
		name, path, payload string
		valid               bool
	}{
		{"configuration", "configurations", configuration, true},
		{"cluster", "kube-clusters", cluster, true},
		{"pinned code", "configurations", strings.Replace(configuration, `"source":{`, `"source":{"revision":"`+strings.Repeat("a", 40)+`",`, 1), true},
		{"invalid revision", "configurations", strings.Replace(configuration, `"source":{`, `"source":{"revision":"main",`, 1), false},
		{"empty argv", "configurations", strings.Replace(configuration, `["ansible-playbook","plays/base.yml"]`, `[]`, 1), false},
		{"invalid argv type", "configurations", strings.Replace(configuration, `["ansible-playbook","plays/base.yml"]`, `"ansible-playbook plays/base.yml"`, 1), false},
		{"empty executable", "configurations", strings.Replace(configuration, `"ansible-playbook"`, `""`, 1), false},
		{"Kubernetes context", "configurations", strings.Replace(configuration, `"targetRef":`, `"context":{"kubeClusterRef":{"name":"lab"}},"targetRef":`, 1), true},
		{"spec UID", "configurations", strings.Replace(configuration, `"name":"node"`, `"name":"node","uid":"node-uid"`, 1), false},
		{"named dependency", "configurations", strings.Replace(configuration, `"targetRef":`, `"dependsOn":[{"name":"prerequisite"}],"targetRef":`, 1), true},
		{"floating Kubernetes", "kube-clusters", strings.Replace(cluster, "1.34.1", "latest", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := &fakeStore{}
			handler := New(slog.New(slog.NewTextHandler(io.Discard, nil)), storage, time.Second)
			request := httptest.NewRequest("POST", "/api/v1alpha1/"+test.path, strings.NewReader(test.payload))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if test.valid && response.Code != 201 {
				t.Fatalf("expected creation: %d %s", response.Code, response.Body.String())
			}
			if !test.valid && response.Code < 400 {
				t.Fatalf("unsafe input accepted: %d", response.Code)
			}
		})
	}
}
