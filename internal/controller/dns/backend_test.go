package dns

import (
	"reflect"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
)

func TestRouterOSRecordValues(t *testing.T) {
	tests := []struct {
		name       string
		recordType apigen.DNSRecordSpecType
		value      string
		want       map[string]string
		wantError  bool
	}{
		{name: "A", recordType: apigen.DNSRecordTypeA, value: "192.0.2.10", want: map[string]string{"address": "192.0.2.10"}},
		{name: "AAAA", recordType: apigen.DNSRecordTypeAAAA, value: "2001:db8::10", want: map[string]string{"address": "2001:db8::10"}},
		{name: "CNAME", recordType: apigen.DNSRecordTypeCNAME, value: "target.example.com", want: map[string]string{"cname": "target.example.com"}},
		{name: "MX", recordType: apigen.DNSRecordTypeMX, value: "10 mail.example.com", want: map[string]string{"mx-preference": "10", "mx-exchange": "mail.example.com"}},
		{name: "NS", recordType: apigen.DNSRecordTypeNS, value: "ns1.example.com", want: map[string]string{"ns": "ns1.example.com"}},
		{name: "SRV", recordType: apigen.DNSRecordTypeSRV, value: "10 20 443 service.example.com", want: map[string]string{"srv-priority": "10", "srv-weight": "20", "srv-port": "443", "srv-target": "service.example.com"}},
		{name: "TXT", recordType: apigen.DNSRecordTypeTXT, value: "hello world", want: map[string]string{"text": "hello world"}},
		{name: "invalid A", recordType: apigen.DNSRecordTypeA, value: "2001:db8::10", wantError: true},
		{name: "invalid MX", recordType: apigen.DNSRecordTypeMX, value: "mail.example.com", wantError: true},
		{name: "unsupported CAA", recordType: apigen.DNSRecordTypeCAA, value: "0 issue letsencrypt.org", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := routerOSRecordValues(test.recordType, test.value)
			if test.wantError {
				if err == nil {
					t.Fatalf("routerOSRecordValues() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("routerOSRecordValues() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("routerOSRecordValues() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestFQDN(t *testing.T) {
	tests := []struct {
		name, zone, want string
	}{
		{name: "host", zone: "example.com", want: "host.example.com"},
		{name: "host.example.com", zone: "example.com", want: "host.example.com"},
		{name: "other.example.net.", zone: "example.com", want: "other.example.net"},
		{name: "@", zone: "example.com.", want: "example.com"},
	}
	for _, test := range tests {
		if got := fqdn(test.name, test.zone); got != test.want {
			t.Errorf("fqdn(%q, %q) = %q, want %q", test.name, test.zone, got, test.want)
		}
	}
}
