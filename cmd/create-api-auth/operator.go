package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/asdf57/stigmergy/internal/api"
)

// Prepare an independent private policy directory without rotating any token
// or overwriting the source. Add public dependency and execution-state reads.
func prepareOperatorPolicy(source, directory string) error {
	policy, err := api.LoadAccessPolicy(source)
	if err != nil {
		return err
	}
	tokens := map[string]string{}
	runnerFound := false
	for i := range policy.Identities {
		identity := &policy.Identities[i]
		tokens[identity.Name] = identity.Token
		if identity.Name != "runner" {
			continue
		}
		runnerFound = true
		for _, kind := range []string{"SSHKeyPair", "Machine", "ISO", "SSHCertificateAuthority", "Command", "ProvisioningRun"} {
			permitted := false
			for _, permission := range identity.Permissions {
				if permission.Kind == kind && permission.Subresource == "" && len(permission.ResourceNames) == 0 {
					for _, method := range permission.Methods {
						if method == "GET" {
							permitted = true
						}
					}
				}
			}
			if !permitted {
				identity.Permissions = append(identity.Permissions, api.AccessPermission{Kind: kind, Methods: []string{"GET"}})
			}
		}
		permitted := false
		for _, permission := range identity.Permissions {
			if permission.Kind == "ProvisioningRun" && permission.Subresource == "status" && len(permission.ResourceNames) == 0 {
				for _, method := range permission.Methods {
					if method == "PATCH" {
						permitted = true
					}
				}
			}
		}
		if !permitted {
			identity.Permissions = append(identity.Permissions, api.AccessPermission{Kind: "ProvisioningRun", Subresource: "status", Methods: []string{"PATCH"}})
		}
	}
	if !runnerFound || tokens["admin"] == "" || tokens["agent"] == "" {
		return fmt.Errorf("admin, agent and runner identities are required")
	}
	document, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return fmt.Errorf("output directory must be fresh: %w", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "api-access.json"), append(document, '\n'), 0600); err != nil {
		return err
	}
	for _, name := range []string{"admin", "agent", "runner"} {
		if err := os.WriteFile(filepath.Join(directory, name+"-token"), []byte(tokens[name]+"\n"), 0600); err != nil {
			return err
		}
	}
	return nil
}
