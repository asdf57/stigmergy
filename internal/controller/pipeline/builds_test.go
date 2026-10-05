package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
)

func TestSharedJobExecutionUsesFlyLoginAndJSONBuildIDs(t *testing.T) {
	dir := t.TempDir()
	executable, log := filepath.Join(dir, "fly"), filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FLY_TEST_LOG"
if [ "$1" = "login" ]; then
  printf 'targets:\n  stigmergy:\n    token:\n      type: Bearer\n      value: test-token\n' > "$HOME/.flyrc"
  exit 0
fi
if [ "$3" = "builds" ]; then printf '[{"id":500,"status":"succeeded"},{"id":42,"status":"started"}]'; fi
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLY_TEST_LOG", log)
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing fly authentication")
			w.WriteHeader(401)
			return
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/teams/main/pipelines/commands-servers/jobs/run/builds":
			w.Write([]byte(`{"id":501,"status":"pending"}`))
		case "GET /api/v1/builds/42":
			w.Write([]byte(`{"id":42,"status":"started","pipeline_name":"commands-servers","job_name":"run","team_name":"main"}`))
		case "GET /api/v1/builds/99":
			w.Write([]byte(`{"id":99,"status":"started","pipeline_name":"other","job_name":"run","team_name":"main"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	b := &FlyBackend{executable: executable}
	p := registry.NewPipelineProvider(resource.Metadata{}, apigen.PipelineProviderSpec{Url: server.URL})
	c := registry.NewUsernamePasswordCredential(resource.Metadata{}, apigen.UsernamePasswordCredentialSpec{Username: "test", Password: "test"})
	ctx := context.Background()
	if err := b.CheckResource(ctx, p, c, "commands-servers", "commands", map[string]string{"ref": "abc123"}); err != nil {
		t.Fatal(err)
	}
	build, err := b.Trigger(ctx, p, c, "commands-servers")
	if err != nil || build.ID != 501 {
		t.Fatal(build, err)
	}
	builds, err := b.Builds(ctx, p, c, "commands-servers")
	if err != nil || len(builds) != 2 {
		t.Fatal(builds, err)
	}
	build, err = b.Build(ctx, p, c, "commands-servers", 42)
	if err != nil || build.ID != 42 {
		t.Fatal(build, err)
	}
	if _, err := b.Build(ctx, p, c, "commands-servers", 99); err == nil {
		t.Fatal("accepted a different executor's build")
	}
	if err := b.Abort(ctx, p, c, 42); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	for _, expected := range []string{"login -t stigmergy", "check-resource --resource commands-servers/commands --from ref:abc123", "builds --job commands-servers/run --count 100 --json", "abort-build --build 42"} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	fail = true
	if _, err := b.Trigger(ctx, p, c, "commands-servers"); err == nil {
		t.Fatal("failed submission accepted")
	}
}
