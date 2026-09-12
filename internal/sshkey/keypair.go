package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateEd25519KeyPair returns an OpenSSH private key, authorized-key public
// key, and SHA256 fingerprint for one newly generated key pair.
func GenerateEd25519KeyPair() (string, string, string, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate Ed25519 key: %w", err)
	}
	sshPublicKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		return "", "", "", fmt.Errorf("encode Ed25519 public key: %w", err)
	}
	privateBlock, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		return "", "", "", fmt.Errorf("encode Ed25519 private key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(privateBlock)
	if privatePEM == nil {
		return "", "", "", errors.New("encode Ed25519 private key PEM")
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublicKey)))
	return string(privatePEM), public, ssh.FingerprintSHA256(sshPublicKey), nil
}
