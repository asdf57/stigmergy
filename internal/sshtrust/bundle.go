// Package sshtrust handles public trust content independently of controllers.
package sshtrust

import (
	"crypto/sha256"
	"fmt"
	"golang.org/x/crypto/ssh"
	"sort"
	"strings"
)

// CanonicalKey rejects options, certificates and trailing keys, ignoring comments.
func CanonicalKey(value string) (string, string, error) {
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil {
		return "", "", fmt.Errorf("parse CA public key: %w", err)
	}
	if len(options) != 0 || strings.TrimSpace(string(rest)) != "" || key.Type() != ssh.KeyAlgoED25519 {
		return "", "", fmt.Errorf("CA trust requires one plain Ed25519 public key")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), ssh.FingerprintSHA256(key), nil
}

func Bundle(keys []string) (string, string, error) {
	unique := map[string]bool{}
	for _, key := range keys {
		canonical, _, err := CanonicalKey(key)
		if err != nil {
			return "", "", err
		}
		unique[canonical] = true
	}
	if len(unique) == 0 {
		return "", "", fmt.Errorf("CA trust bundle is empty")
	}
	ordered := make([]string, 0, len(unique))
	for key := range unique {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	content := strings.Join(ordered, "\n") + "\n"
	return content, fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(content))), nil
}
