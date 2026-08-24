package sshaccess

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

type KeyOwnership struct {
	ServerUID string
	GrantUID  string
}

type KeyPair struct {
	PublicKey   string
	Fingerprint string
	Version     int64
}

type KeyStore interface {
	EnsureKeyPair(context.Context, apigen.OpenBaoSecretStoreProvider, string, KeyOwnership) (KeyPair, error)
	DeleteKeyPair(context.Context, apigen.OpenBaoSecretStoreProvider, string, KeyOwnership) error
}

type OpenBaoKeyStore struct {
	client    *http.Client
	lookupEnv func(string) (string, bool)
	readFile  func(string) ([]byte, error)
}

func NewOpenBaoKeyStore() *OpenBaoKeyStore {
	return &OpenBaoKeyStore{
		client:    &http.Client{Timeout: 10 * time.Second},
		lookupEnv: os.LookupEnv,
		readFile:  os.ReadFile,
	}
}

func (s *OpenBaoKeyStore) EnsureKeyPair(ctx context.Context, provider apigen.OpenBaoSecretStoreProvider, logicalPath string, ownership KeyOwnership) (KeyPair, error) {
	token, err := s.authenticationToken(provider.Authentication)
	if err != nil {
		return KeyPair{}, err
	}
	endpoint, err := openBaoKVEndpoint(provider, logicalPath)
	if err != nil {
		return KeyPair{}, err
	}

	pair, found, err := s.read(ctx, endpoint, token, ownership)
	if err != nil || found {
		return pair, err
	}
	privateKey, publicKey, fingerprint, err := generateEd25519KeyPair()
	if err != nil {
		return KeyPair{}, err
	}
	payload := map[string]any{
		"options": map[string]any{"cas": 0},
		"data": map[string]any{
			"privateKey":     privateKey,
			"publicKey":      publicKey,
			"serverUID":      ownership.ServerUID,
			"accessGrantUID": ownership.GrantUID,
		},
	}
	if err := s.write(ctx, endpoint, token, payload); err != nil {
		// A competing reconciler may have won the create-only write.
		if existing, exists, readErr := s.read(ctx, endpoint, token, ownership); readErr == nil && exists {
			return existing, nil
		}
		return KeyPair{}, err
	}
	created, found, err := s.read(ctx, endpoint, token, ownership)
	if err != nil {
		return KeyPair{}, err
	}
	if !found {
		return KeyPair{}, errors.New("OpenBao key pair was not readable after creation")
	}
	if created.PublicKey != publicKey || created.Fingerprint != fingerprint {
		return KeyPair{}, errors.New("OpenBao returned different key material after creation")
	}
	return created, nil
}

func (s *OpenBaoKeyStore) DeleteKeyPair(ctx context.Context, provider apigen.OpenBaoSecretStoreProvider, logicalPath string, ownership KeyOwnership) error {
	token, err := s.authenticationToken(provider.Authentication)
	if err != nil {
		return err
	}
	dataEndpoint, err := openBaoKVEndpoint(provider, logicalPath)
	if err != nil {
		return err
	}
	_, found, err := s.read(ctx, dataEndpoint, token, ownership)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	metadataEndpoint, err := openBaoKVMetadataEndpoint(provider, logicalPath)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, metadataEndpoint, nil)
	if err != nil {
		return fmt.Errorf("create OpenBao delete request: %w", err)
	}
	request.Header.Set("X-Vault-Token", token)
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("delete OpenBao key pair: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return openBaoResponseError("delete key pair", response)
	}
	return nil
}

func (s *OpenBaoKeyStore) authenticationToken(authentication apigen.OpenBaoSecretStoreAuthentication) (string, error) {
	switch {
	case authentication.TokenFile != nil && authentication.TokenEnvironmentVariable == nil:
		encoded, err := s.readFile(*authentication.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read OpenBao token file %q: %w", *authentication.TokenFile, err)
		}
		token := strings.TrimSpace(string(encoded))
		if token == "" {
			return "", fmt.Errorf("OpenBao token file %q is empty", *authentication.TokenFile)
		}
		return token, nil
	case authentication.TokenEnvironmentVariable != nil && authentication.TokenFile == nil:
		token, found := s.lookupEnv(*authentication.TokenEnvironmentVariable)
		if !found || token == "" {
			return "", fmt.Errorf("OpenBao token environment variable %q is not set", *authentication.TokenEnvironmentVariable)
		}
		return token, nil
	default:
		return "", errors.New("OpenBao authentication must configure exactly one of tokenFile or tokenEnvironmentVariable")
	}
}

func (s *OpenBaoKeyStore) read(ctx context.Context, endpoint, token string, ownership KeyOwnership) (KeyPair, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return KeyPair{}, false, fmt.Errorf("create OpenBao read request: %w", err)
	}
	request.Header.Set("X-Vault-Token", token)
	response, err := s.client.Do(request)
	if err != nil {
		return KeyPair{}, false, fmt.Errorf("read OpenBao key pair: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return KeyPair{}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return KeyPair{}, false, openBaoResponseError("read key pair", response)
	}
	var body struct {
		Data struct {
			Data     map[string]any `json:"data"`
			Metadata struct {
				Version int64 `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return KeyPair{}, false, fmt.Errorf("decode OpenBao key pair response: %w", err)
	}
	serverUID, _ := body.Data.Data["serverUID"].(string)
	grantUID, _ := body.Data.Data["accessGrantUID"].(string)
	if serverUID != ownership.ServerUID || grantUID != ownership.GrantUID {
		return KeyPair{}, false, fmt.Errorf("OpenBao secret ownership conflict: stored server UID %q and grant UID %q do not match", serverUID, grantUID)
	}
	publicKey, _ := body.Data.Data["publicKey"].(string)
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return KeyPair{}, false, fmt.Errorf("parse public key stored in OpenBao: %w", err)
	}
	privateKey, _ := body.Data.Data["privateKey"].(string)
	parsedPrivateKey, err := ssh.ParseRawPrivateKey([]byte(privateKey))
	if err != nil {
		return KeyPair{}, false, fmt.Errorf("parse private key stored in OpenBao: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(parsedPrivateKey)
	if err != nil {
		return KeyPair{}, false, fmt.Errorf("derive public key from OpenBao private key: %w", err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), parsed.Marshal()) {
		return KeyPair{}, false, errors.New("public and private keys stored in OpenBao do not match")
	}
	if body.Data.Metadata.Version < 1 {
		return KeyPair{}, false, errors.New("OpenBao key pair response does not contain a valid version")
	}
	return KeyPair{PublicKey: strings.TrimSpace(publicKey), Fingerprint: ssh.FingerprintSHA256(parsed), Version: body.Data.Metadata.Version}, true, nil
}

func (s *OpenBaoKeyStore) write(ctx context.Context, endpoint, token string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode OpenBao key pair: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create OpenBao write request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Vault-Token", token)
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("write OpenBao key pair: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return openBaoResponseError("create key pair", response)
	}
	return nil
}

func openBaoKVEndpoint(provider apigen.OpenBaoSecretStoreProvider, logicalPath string) (string, error) {
	return openBaoKVAPIEndpoint(provider, "data", logicalPath)
}

func openBaoKVMetadataEndpoint(provider apigen.OpenBaoSecretStoreProvider, logicalPath string) (string, error) {
	return openBaoKVAPIEndpoint(provider, "metadata", logicalPath)
}

func openBaoKVAPIEndpoint(provider apigen.OpenBaoSecretStoreProvider, operation, logicalPath string) (string, error) {
	base, err := url.Parse(provider.Address)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("invalid OpenBao address %q", provider.Address)
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return "", fmt.Errorf("OpenBao address %q cannot contain a query or fragment", provider.Address)
	}
	segments := append([]string{provider.KvV2Mount, operation}, strings.Split(logicalPath, "/")...)
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("OpenBao path %q is not canonical", logicalPath)
		}
	}
	escaped := make([]string, len(segments))
	for index, segment := range segments {
		escaped[index] = url.PathEscape(segment)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/v1/" + strings.Join(escaped, "/")
	return base.String(), nil
}

func generateEd25519KeyPair() (string, string, string, error) {
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
	authorizedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublicKey)))
	return string(privatePEM), authorizedKey, ssh.FingerprintSHA256(sshPublicKey), nil
}

func openBaoResponseError(operation string, response *http.Response) error {
	var body struct {
		Errors []string `json:"errors"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body)
	detail := strings.Join(body.Errors, "; ")
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("OpenBao %s failed with HTTP %d: %s", operation, response.StatusCode, detail)
}

func logicalKeyPath(prefix, serverName, grantName string) (string, error) {
	segments := []string{}
	if prefix != "" {
		segments = append(segments, strings.Split(strings.Trim(prefix, "/"), "/")...)
	}
	segments = append(segments, serverName, "ssh-keys", grantName)
	joined := strings.Join(segments, "/")
	if joined == "" || path.Clean(joined) != joined || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf("secret key prefix %q is not canonical", prefix)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("secret key path %q is not canonical", joined)
		}
	}
	return joined, nil
}
