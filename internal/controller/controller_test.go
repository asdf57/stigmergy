package controller

import (
	"testing"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestEtcdEventsToRequests(t *testing.T) {
	response := clientv3.WatchResponse{
		Events: []*clientv3.Event{
			{Type: mvccpb.PUT, Kv: &mvccpb.KeyValue{Key: []byte("/homelab/v1/resources/MachineReport/alpha")}},
			{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: []byte("/homelab/v1/resources/MachineReport/beta")}},
		},
	}

	requests, err := EtcdEventsToRequests(response)
	if err != nil {
		t.Fatalf("EtcdEventsToRequests() error = %v", err)
	}

	want := []Request{
		{Kind: "MachineReport", Name: "alpha"},
		{Kind: "MachineReport", Name: "beta"},
	}
	if len(requests) != len(want) {
		t.Fatalf("EtcdEventsToRequests() returned %d requests, want %d", len(requests), len(want))
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Errorf("EtcdEventsToRequests()[%d] = %#v, want %#v", i, requests[i], want[i])
		}
	}
}

func TestEtcdEventsToRequestsRejectsEventWithoutKeyValue(t *testing.T) {
	response := clientv3.WatchResponse{Events: []*clientv3.Event{nil}}

	if _, err := EtcdEventsToRequests(response); err == nil {
		t.Fatal("EtcdEventsToRequests() error = nil, want an error")
	}
}
