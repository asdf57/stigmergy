package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRegistryGeneratesTypedStatusOnlyWhenDeclared(t *testing.T) {
	output := filepath.Join(t.TempDir(), "registry.gen.go")
	writeRegistry(output, "example.test/models", "example.test/resource", []resourceModule{
		{Metadata: resourceMetadata{Kind: "Widget", SpecSchema: "WidgetSpec", StatusSchema: "WidgetStatus"}},
		{Metadata: resourceMetadata{Kind: "Report", SpecSchema: "ReportSpec"}},
	})

	encoded, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read generated registry: %v", err)
	}
	generated := string(encoded)
	for _, expected := range []string{
		"*apigen.WidgetStatus `json:\"status,omitempty\"`",
		"decodeStoredStatus[apigen.WidgetStatus]",
		"func (definition WidgetDefinition) EncodeStatus(status *apigen.WidgetStatus)",
		"value.Spec, value.Status)",
		"value.Spec, nil)",
	} {
		if !strings.Contains(generated, expected) {
			t.Fatalf("generated registry does not contain %q:\n%s", expected, generated)
		}
	}
	if strings.Contains(generated, "Status map[string]any") {
		t.Fatalf("generated concrete resource contains untyped status:\n%s", generated)
	}
	reportStart := strings.Index(generated, "type Report struct {")
	if reportStart < 0 {
		t.Fatalf("generated Report declaration is missing:\n%s", generated)
	}
	reportEnd := strings.Index(generated[reportStart:], "}\n")
	if reportEnd < 0 {
		t.Fatalf("generated Report declaration is unterminated:\n%s", generated)
	}
	if strings.Contains(generated[reportStart:reportStart+reportEnd], "Status") {
		t.Fatalf("status-less Report unexpectedly has a Status field:\n%s", generated[reportStart:reportStart+reportEnd])
	}
}
