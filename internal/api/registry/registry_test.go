package registry

import (
	"strings"
	"testing"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
)

func TestGeneratedResourceDefinitionDecodesTypedSpec(t *testing.T) {
	raw := resource.Resource{
		APIVersion: MachineReportResource.APIVersion,
		Kind:       MachineReportResource.Kind,
		Metadata:   resource.Metadata{Name: "lab-node"},
		Spec: map[string]any{
			"observed_at": "2026-08-18T12:00:00Z",
			"storage":     []any{},
			"system":      map[string]any{},
			"cpu": map[string]any{
				"model_name": "Test CPU",
				"cores":      []any{},
			},
			"interfaces": []any{},
			"lldp_info":  []any{},
		},
	}

	report, err := MachineReportResource.Decode(raw)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if report.Metadata.Name != "lab-node" {
		t.Fatalf("Metadata.Name = %q, want lab-node", report.Metadata.Name)
	}
	if report.Spec.Cpu.ModelName != "Test CPU" {
		t.Fatalf("Spec.Cpu.ModelName = %q, want Test CPU", report.Spec.Cpu.ModelName)
	}
}

func TestGeneratedResourceEncodesTypedSpec(t *testing.T) {
	typed := NewMachine(
		resource.Metadata{Name: "lab-node"},
		apigen.MachineSpec{
			Location: apigen.MachineLocation{
				LldpPort:  "Ethernet1",
				SwitchMac: "00:11:22:33:44:55",
			},
		},
	)

	stored, err := typed.Encode()
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if stored.APIVersion != MachineResource.APIVersion || stored.Kind != MachineResource.Kind {
		t.Fatalf("Encode() identity = %s %s, want %s %s", stored.APIVersion, stored.Kind, MachineResource.APIVersion, MachineResource.Kind)
	}
	location, ok := stored.Spec["location"].(map[string]any)
	if !ok {
		t.Fatalf("Encode() location = %#v, want object", stored.Spec["location"])
	}
	if location["lldp_port"] != "Ethernet1" || location["switch_mac"] != "00:11:22:33:44:55" {
		t.Fatalf("Encode() location = %#v, want LLDP port and switch MAC", location)
	}
	if _, exists := stored.Spec["SourceReport"]; exists {
		t.Fatal("Encode() used a Go field name instead of its generated JSON field name")
	}
}

func TestGeneratedResourceLiteralDefaultsIdentityWhenEncoded(t *testing.T) {
	typed := Machine{
		Metadata: resource.Metadata{Name: "lab-node"},
		Spec: apigen.MachineSpec{
			Location: apigen.MachineLocation{},
		},
	}

	stored, err := typed.Encode()
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if stored.APIVersion != MachineResource.APIVersion || stored.Kind != MachineResource.Kind {
		t.Fatalf("Encode() identity = %s %s, want %s %s", stored.APIVersion, stored.Kind, MachineResource.APIVersion, MachineResource.Kind)
	}
}

func TestGeneratedResourceRoundTrip(t *testing.T) {
	original := NewMachine(
		resource.Metadata{Name: "lab-node"},
		apigen.MachineSpec{Location: apigen.MachineLocation{}},
	)
	stored, err := original.Encode()
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := MachineResource.Decode(stored)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.APIVersion != original.APIVersion || decoded.Kind != original.Kind {
		t.Fatalf("Decode() identity = %s %s, want %s %s", decoded.APIVersion, decoded.Kind, original.APIVersion, original.Kind)
	}
	if decoded.Spec.Location != original.Spec.Location {
		t.Fatalf("Decode() location = %#v, want %#v", decoded.Spec.Location, original.Spec.Location)
	}
}

func TestGeneratedResourceDefinitionRejectsWrongIdentity(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		want       string
	}{
		{name: "api version", apiVersion: "other.io/v1", kind: MachineReportResource.Kind, want: "expected apiVersion"},
		{name: "kind", apiVersion: MachineReportResource.APIVersion, kind: "Other", want: "expected kind"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := MachineReportResource.Decode(resource.Resource{
				APIVersion: test.apiVersion,
				Kind:       test.kind,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Decode() error = %v, want error containing %q", err, test.want)
			}
		})
	}
}
