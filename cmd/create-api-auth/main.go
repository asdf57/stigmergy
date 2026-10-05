// create-api-auth generates local standup credentials, never checked-in defaults.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/asdf57/stigmergy/internal/api"
)

func generate(directory string) error {
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		return err
	}
	// Refuse regeneration, including an existing empty directory or symlink.
	if err := os.Mkdir(directory, 0700); err != nil {
		return fmt.Errorf("create fresh credentials directory (never overwrite existing identities): %w", err)
	}
	policy := api.AccessPolicy{}
	permissions := map[string][]api.AccessPermission{
		"admin":  {{Kind: "*", Methods: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}}},
		"agent":  {{Kind: "MachineReport", Methods: []string{"POST"}}, {Kind: "Server", Methods: []string{"GET"}}},
		"runner": {{Kind: "InventoryCaptureGroup", Methods: []string{"GET"}}, {Kind: "Server", Methods: []string{"GET"}}, {Kind: "Server", Subresource: "status", Methods: []string{"PATCH"}}},
	}
	for _, name := range []string{"admin", "agent", "runner"} {
		bytes := make([]byte, 32)
		if _, err := rand.Read(bytes); err != nil {
			return err
		}
		token := hex.EncodeToString(bytes)
		policy.Identities = append(policy.Identities, api.AccessIdentity{Name: name, Token: token, Permissions: permissions[name]})
		if err := os.WriteFile(filepath.Join(directory, name+"-token"), []byte(token+"\n"), 0600); err != nil {
			return err
		}
	}
	document, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "api-access.json"), append(document, '\n'), 0600); err != nil {
		return err
	}
	// Docker --env-file accepts this single-line JSON verbatim; do not shell-source it.
	environment := fmt.Sprintf("STIGMERGY_API_TOKEN=%s\nSTIGMERGY_API_POLICY=%s\nSTIGMERGY_RUNNER_API_TOKEN=%s\nSTIGMERGY_AGENT_API_TOKEN=%s\n", policy.Identities[0].Token, document, policy.Identities[2].Token, policy.Identities[1].Token)
	return os.WriteFile(filepath.Join(directory, "bootstrap.env"), []byte(environment), 0600)
}

func main() {
	directory := flag.String("output-dir", ".local/api-auth", "fresh private output directory")
	source := flag.String("bootstrap-env-source", "", "existing Docker env-file to combine with the private API credentials")
	output := flag.String("bootstrap-env-output", "", "fresh private combined Docker env-file (never overwrites)")
	flag.Parse()
	if *source != "" || *output != "" {
		if err := prepareBootstrap(*directory, *source, *output); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Prepared private bootstrap env-file; no credentials printed.")
		return
	}
	if err := generate(*directory); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Created API policy, independent token files, and bootstrap.env in %s (no credentials printed).\n", *directory)
}
