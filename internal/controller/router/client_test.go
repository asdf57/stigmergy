package router

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	routeros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

func TestParseRouterOSAddresses(t *testing.T) {
	reply := &routeros.Reply{Re: []*proto.Sentence{
		{Map: map[string]string{
			".id": "*1", "address": "10.0.3.1/24", "network": "10.0.3.0",
			"interface": "ether2", "actual-interface": "bridge", "comment": "stigmergy:metallb",
			"disabled": "false", "invalid": "false", "dynamic": "false", "slave": "true",
		}},
		{Map: map[string]string{
			".id": "*2", "address": "10.0.0.178/24", "network": "10.0.0.0",
			"interface": "ISP", "actual-interface": "ISP", "dynamic": "true",
			"future-field": "ignored",
		}},
	}}
	want := []RouterOSAddress{
		{ID: "*1", Address: "10.0.3.1/24", Network: "10.0.3.0", Interface: "ether2",
			ActualInterface: "bridge", Comment: "stigmergy:metallb", Slave: true},
		{ID: "*2", Address: "10.0.0.178/24", Network: "10.0.0.0", Interface: "ISP",
			ActualInterface: "ISP", Dynamic: true},
	}
	got, err := parseRouterOSAddresses(reply)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %#v, want %#v", got, want)
	}
}

func TestParseRouterOSAddressesRejectsMalformedFlags(t *testing.T) {
	for _, flag := range []string{"disabled", "invalid", "dynamic", "slave"} {
		t.Run(flag, func(t *testing.T) {
			reply := &routeros.Reply{Re: []*proto.Sentence{
				{Map: map[string]string{".id": "*1", "address": "10.0.3.1/24"}},
				{Map: map[string]string{".id": "*2", flag: "not-a-boolean"}},
			}}
			got, err := parseRouterOSAddresses(reply)
			if err == nil || !strings.Contains(err.Error(), `address "*2" flag `+flag) {
				t.Fatalf("error = %v, want address identity and flag name", err)
			}
			if got != nil {
				t.Fatalf("returned partial addresses after malformed input: %#v", got)
			}
		})
	}
}

func TestParseRouterOSAddressesEmptyReply(t *testing.T) {
	got, err := parseRouterOSAddresses(&routeros.Reply{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("addresses = %#v, error = %v; want empty non-nil slice", got, err)
	}
}

func TestRouterClientHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := NewRouterOSApi("127.0.0.1:1", "unused", "unused")
	if err := client.Probe(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe error = %v, want canceled context", err)
	}
	if _, err := client.ListAddresses(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListAddresses error = %v, want canceled context", err)
	}
}

func TestAddressArgumentsOnlyWriteConfigurableFields(t *testing.T) {
	address := RouterOSAddress{Address: "10.0.3.1/24", Network: "10.0.3.0", Interface: "bridge",
		Comment: "stigmergy:router:uid:address:metallb", ActualInterface: "read-only", Dynamic: true}
	want := []string{"=address=10.0.3.1/24", "=network=10.0.3.0", "=interface=bridge",
		"=comment=stigmergy:router:uid:address:metallb", "=disabled=false"}
	if got := addressArguments(address); !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %v, want %v", got, want)
	}
}
