package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
)

// ---
// apiVersion: homelab.io/v1alpha1
// kind: SecretStore
// metadata:
//   name: openbao
// spec:
//   provider:
//     openBao:
//       address: http://openbao:8200
//       kvV2Mount: kv2
//       keyPrefix: secrets
//       authentication:
//         tokenFile: /run/openbao/token

type OpenBaoKeyStore struct {
	client   *http.Client
	provider apigen.OpenBaoSecretStoreProvider
}

func NewOpenBaoKeyStore(provider *apigen.OpenBaoSecretStoreProvider) *OpenBaoKeyStore {
	slog.Info("creating OpenBao key store",
		"address", provider.Address,
		"kvV2Mount", provider.KvV2Mount,
	)
	return &OpenBaoKeyStore{
		client:   &http.Client{Timeout: 10 * time.Second},
		provider: *provider,
	}
}

func (s *OpenBaoKeyStore) getAuthToken() (string, error) {
	switch {
	case s.provider.Authentication.TokenFile != nil && s.provider.Authentication.TokenEnvironmentVariable == nil:
		slog.Info("loading OpenBao authentication token from file", "tokenFile", *s.provider.Authentication.TokenFile)
		encoded, err := os.ReadFile(*s.provider.Authentication.TokenFile)
		if err != nil {
			slog.Error("failed to read OpenBao authentication token file", "tokenFile", *s.provider.Authentication.TokenFile, "error", err)
			return "", fmt.Errorf("read OpenBao token file %q: %w", *s.provider.Authentication.TokenFile, err)
		}
		token := strings.TrimSpace(string(encoded))
		if token == "" {
			slog.Error("OpenBao authentication token file is empty", "tokenFile", *s.provider.Authentication.TokenFile)
			return "", fmt.Errorf("OpenBao token file %q is empty", *s.provider.Authentication.TokenFile)
		}
		slog.Info("loaded OpenBao authentication token from file", "tokenFile", *s.provider.Authentication.TokenFile)
		return token, nil
	case s.provider.Authentication.TokenEnvironmentVariable != nil && s.provider.Authentication.TokenFile == nil:
		slog.Info("loading OpenBao authentication token from environment", "environmentVariable", *s.provider.Authentication.TokenEnvironmentVariable)
		token := os.Getenv(*s.provider.Authentication.TokenEnvironmentVariable)
		if token == "" {
			slog.Error("OpenBao authentication token environment variable is not set", "environmentVariable", *s.provider.Authentication.TokenEnvironmentVariable)
			return "", fmt.Errorf("OpenBao token environment variable %q is not set", *s.provider.Authentication.TokenEnvironmentVariable)
		}
		slog.Info("loaded OpenBao authentication token from environment", "environmentVariable", *s.provider.Authentication.TokenEnvironmentVariable)
		return token, nil
	default:
		slog.Error("invalid OpenBao authentication configuration")
		return "", errors.New("OpenBao authentication must configure exactly one of tokenFile or tokenEnvironmentVariable")
	}
}

func (s *OpenBaoKeyStore) ReadSecret(ctx context.Context, path, key string) (string, bool, error) {
	logger := slog.With("path", path, "key", key)
	logger.InfoContext(ctx, "reading secret from OpenBao")
	queryUrl, err := s.GetOpenBaoKVEndpoint(path)
	if err != nil {
		logger.ErrorContext(ctx, "failed to build OpenBao KV read endpoint", "error", err)
		return "", false, fmt.Errorf("get OpenBao KV read endpoint: %w", err)
	}
	logger = logger.With("url", queryUrl)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, queryUrl, nil)
	if err != nil {
		logger.ErrorContext(ctx, "failed to create OpenBao read request", "error", err)
		return "", false, fmt.Errorf("create OpenBao read request: %w", err)
	}
	token, err := s.getAuthToken()
	if err != nil {
		logger.ErrorContext(ctx, "failed to authenticate OpenBao read request", "error", err)
		return "", false, fmt.Errorf("get OpenBao auth token: %w", err)
	}
	request.Header.Set("X-Vault-Token", token)
	started := time.Now()
	logger.InfoContext(ctx, "sending OpenBao read request")
	response, err := s.client.Do(request)
	if err != nil {
		logger.ErrorContext(ctx, "OpenBao read request failed", "duration", time.Since(started), "error", err)
		return "", false, fmt.Errorf("read OpenBao secret: %w", err)
	}
	defer response.Body.Close()
	logger.InfoContext(ctx, "received OpenBao read response", "status", response.StatusCode, "duration", time.Since(started))
	if response.StatusCode == http.StatusNotFound {
		logger.InfoContext(ctx, "OpenBao secret was not found")
		return "", false, nil
	}
	if response.StatusCode != http.StatusOK {
		err := openBaoResponseError("read secret", response)
		logger.ErrorContext(ctx, "OpenBao rejected secret read", "status", response.StatusCode, "error", err)
		return "", false, err
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
		logger.ErrorContext(ctx, "failed to decode OpenBao read response", "error", err)
		return "", false, fmt.Errorf("decode OpenBao secret response: %w", err)
	}
	logger.InfoContext(ctx, "read secret from OpenBao", "version", body.Data.Metadata.Version, "fieldCount", len(body.Data.Data))
	return body.Data.Data["secret"].(string), true, nil
}

func (s *OpenBaoKeyStore) WriteSecret(ctx context.Context, path string, payloadMap map[string]string) error {
	logger := slog.With("path", path, "fieldCount", len(payloadMap))
	logger.InfoContext(ctx, "writing secret to OpenBao")
	url, err := s.GetOpenBaoKVEndpoint(path)
	if err != nil {
		logger.ErrorContext(ctx, "failed to build OpenBao KV write endpoint", "error", err)
		return fmt.Errorf("get OpenBao KV endpoint: %w", err)
	}
	logger = logger.With("url", url)

	encoded, err := json.Marshal(map[string]any{"data": payloadMap})
	if err != nil {
		logger.ErrorContext(ctx, "failed to encode OpenBao secret", "error", err)
		return fmt.Errorf("encode OpenBao secret: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		logger.ErrorContext(ctx, "failed to create OpenBao write request", "error", err)
		return fmt.Errorf("create OpenBao write request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	token, err := s.getAuthToken()
	if err != nil {
		logger.ErrorContext(ctx, "failed to authenticate OpenBao write request", "error", err)
		return fmt.Errorf("get OpenBao auth token: %w", err)
	}
	request.Header.Set("X-Vault-Token", token)
	started := time.Now()
	logger.InfoContext(ctx, "sending OpenBao write request", "payloadBytes", len(encoded))
	response, err := s.client.Do(request)
	if err != nil {
		logger.ErrorContext(ctx, "OpenBao write request failed", "duration", time.Since(started), "error", err)
		return fmt.Errorf("write OpenBao secret: %w", err)
	}
	defer response.Body.Close()
	logger.InfoContext(ctx, "received OpenBao write response", "status", response.StatusCode, "duration", time.Since(started))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err := openBaoResponseError("create secret", response)
		logger.ErrorContext(ctx, "OpenBao rejected secret write", "status", response.StatusCode, "error", err)
		return err
	}
	logger.InfoContext(ctx, "wrote secret to OpenBao", "status", response.StatusCode)
	return nil
}

func (s *OpenBaoKeyStore) DeleteSecret(ctx context.Context, path string) error {
	logger := slog.With("path", path)
	logger.InfoContext(ctx, "deleting secret from OpenBao")
	url, err := s.GetOpenBaoKVEndpoint(path)
	if err != nil {
		logger.ErrorContext(ctx, "failed to build OpenBao KV delete endpoint", "error", err)
		return fmt.Errorf("get OpenBao KV endpoint: %w", err)
	}
	logger = logger.With("url", url)

	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		logger.ErrorContext(ctx, "failed to create OpenBao delete request", "error", err)
		return fmt.Errorf("create OpenBao delete request: %w", err)
	}
	token, err := s.getAuthToken()
	if err != nil {
		logger.ErrorContext(ctx, "failed to authenticate OpenBao delete request", "error", err)
		return fmt.Errorf("get OpenBao auth token: %w", err)
	}
	request.Header.Set("X-Vault-Token", token)
	started := time.Now()
	logger.InfoContext(ctx, "sending OpenBao delete request")
	response, err := s.client.Do(request)
	if err != nil {
		logger.ErrorContext(ctx, "OpenBao delete request failed", "duration", time.Since(started), "error", err)
		return fmt.Errorf("delete OpenBao secret: %w", err)
	}
	defer response.Body.Close()
	logger.InfoContext(ctx, "received OpenBao delete response", "status", response.StatusCode, "duration", time.Since(started))
	if response.StatusCode == http.StatusNotFound {
		logger.InfoContext(ctx, "OpenBao secret was already absent")
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err := openBaoResponseError("delete secret", response)
		logger.ErrorContext(ctx, "OpenBao rejected secret delete", "status", response.StatusCode, "error", err)
		return err
	}
	logger.InfoContext(ctx, "deleted secret from OpenBao", "status", response.StatusCode, "duration", time.Since(started))
	return nil
}

func (s *OpenBaoKeyStore) GetOpenBaoKVEndpoint(logicalPath string) (string, error) {
	return s.openBaoKVAPIEndpoint(s.provider, "data", logicalPath)
}

func (s *OpenBaoKeyStore) GetOpenBaoKVMetadataEndpoint(logicalPath string) (string, error) {
	return s.openBaoKVAPIEndpoint(s.provider, "metadata", logicalPath)
}

func (s *OpenBaoKeyStore) openBaoKVAPIEndpoint(provider apigen.OpenBaoSecretStoreProvider, operation, logicalPath string) (string, error) {
	slog.Info("building OpenBao KV API endpoint", "address", provider.Address, "keyPrefix", provider.KeyPrefix, "mount", provider.KvV2Mount, "operation", operation, "path", logicalPath)
	base, err := url.Parse(provider.Address)
	if err != nil || base.Scheme == "" || base.Host == "" {
		slog.Error("invalid OpenBao address", "address", provider.Address, "error", err)
		return "", fmt.Errorf("invalid OpenBao address %q", provider.Address)
	}
	if base.RawQuery != "" || base.Fragment != "" {
		slog.Error("OpenBao address contains a query or fragment", "address", provider.Address)
		return "", fmt.Errorf("OpenBao address %q cannot contain a query or fragment", provider.Address)
	}

	pathSegments := strings.Split(logicalPath, "/")
	if provider.KeyPrefix != nil {
		pathSegments = append(strings.Split(*provider.KeyPrefix, "/"), pathSegments...)
	}

	segments := append(
		[]string{provider.KvV2Mount, operation},
		pathSegments...,
	)

	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			slog.Error("OpenBao logical path is not canonical", "path", logicalPath, "operation", operation)
			return "", fmt.Errorf("OpenBao path %q is not canonical", logicalPath)
		}
	}
	escaped := make([]string, len(segments))
	for index, segment := range segments {
		escaped[index] = url.PathEscape(segment)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/v1/" + strings.Join(escaped, "/")
	slog.Info("built OpenBao KV API endpoint", "operation", operation, "path", logicalPath, "url", base.String())
	return base.String(), nil
}

func openBaoResponseError(operation string, response *http.Response) error {
	slog.Info("decoding OpenBao error response", "operation", operation, "status", response.StatusCode)
	var body struct {
		Errors []string `json:"errors"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body)
	detail := strings.Join(body.Errors, "; ")
	if detail == "" {
		detail = http.StatusText(response.StatusCode)
	}
	slog.Info("decoded OpenBao error response", "operation", operation, "status", response.StatusCode, "errorCount", len(body.Errors))
	return fmt.Errorf("OpenBao %s failed with HTTP %d: %s", operation, response.StatusCode, detail)
}
