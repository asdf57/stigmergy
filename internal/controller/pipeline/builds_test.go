package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
)

func TestExecutionBackendUsesNativeFlyCommands(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "fly")
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FLY_TEST_LOG"
if [ "$1" = "login" ]; then exit 0; fi
if [ "$3" = "trigger-job" ] || [ "$3" = "abort-build" ] || [ "$3" = "check-resource" ]; then exit 0; fi
if [ "$FLY_TEST_FAIL" = "1" ]; then exit 1; fi
case "$FLY_TEST_RESULT" in
 build) printf '[{"id":42,"status":"started"}]' ;;
 list) printf '[{"id":42,"status":"succeeded"}]' ;;
 empty) printf '{}' ;;
 *) exit 0 ;;
esac
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLY_TEST_LOG", log)
	t.Setenv("FLY_TEST_RESULT", "build")
	b := &FlyBackend{executable: executable}
	p := registry.NewPipelineProvider(resource.Metadata{}, apigen.PipelineProviderSpec{Url: "https://ci.example"})
	c := registry.NewUsernamePasswordCredential(resource.Metadata{}, apigen.UsernamePasswordCredentialSpec{Username: "test", Password: "test"})
	if err := b.CheckResource(context.Background(), p, c, "command-uid", "commands", map[string]string{"ref": "abc123"}); err != nil {
		t.Fatal(err)
	}
	build, err := b.Trigger(context.Background(), p, c, "command-uid")
	if err != nil || build.ID != 42 {
		t.Fatal(build, err)
	}
	t.Setenv("FLY_TEST_RESULT", "list")
	builds, err := b.Builds(context.Background(), p, c, "command-uid")
	if err != nil || len(builds) != 1 || !builds[0].Terminal() {
		t.Fatal(builds, err)
	}
	t.Setenv("FLY_TEST_RESULT", "build")
	if _, err := b.Build(context.Background(), p, c, "command-uid", 42); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background(), p, c, "command-uid", 99); err == nil {
		t.Fatal("mismatched build ID accepted")
	}
	t.Setenv("FLY_TEST_RESULT", "")
	if err := b.Abort(context.Background(), p, c, 42); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"login -t stigmergy -c https://ci.example -n main", "check-resource --resource command-uid/commands --from ref:abc123", "trigger-job -j command-uid/run", "builds --job command-uid/run --count 2 --json", "abort-build --build 42"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("missing %q in %s", expected, data)
		}
	}
	t.Setenv("FLY_TEST_RESULT", "empty")
	if _, err := b.Trigger(context.Background(), p, c, "command-uid"); err == nil {
		t.Fatal("missing build ID accepted")
	}
	t.Setenv("FLY_TEST_FAIL", "1")
	if _, err := b.Trigger(context.Background(), p, c, "command-uid"); err == nil {
		t.Fatal("failed dispatch accepted")
	}
}
