package main

import (
	"encoding/json"
	"fmt"
	"github.com/asdf57/stigmergy/internal/api"
	"os"
	"path/filepath"
	"strings"
)

// Docker env-files are literal records, not shell scripts. Preserve unrelated
// settings without evaluating them, and never expose secret values in output.
func prepareBootstrap(directory, source, output string) error {
	if source == "" || output == "" {
		return fmt.Errorf("both bootstrap env source and output are required")
	}
	policy, err := api.LoadAccessPolicy(filepath.Join(directory, "api-access.json"))
	if err != nil {
		return err
	}
	tokens := map[string]string{}
	for _, identity := range policy.Identities {
		tokens[identity.Name] = identity.Token
	}
	if tokens["admin"] == "" || tokens["runner"] == "" || tokens["agent"] == "" {
		return fmt.Errorf("admin, runner and agent identities are required")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		key, _, _ := strings.Cut(line, "=")
		switch key {
		case "STIGMERGY_API_TOKEN", "STIGMERGY_API_POLICY", "STIGMERGY_RUNNER_API_TOKEN", "STIGMERGY_AGENT_API_TOKEN":
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	document, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	lines = append(lines, "STIGMERGY_API_TOKEN="+tokens["admin"], "STIGMERGY_API_POLICY="+string(document), "STIGMERGY_RUNNER_API_TOKEN="+tokens["runner"], "STIGMERGY_AGENT_API_TOKEN="+tokens["agent"])
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
