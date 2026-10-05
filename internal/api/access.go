package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/asdf57/stigmergy/internal/api/registry"
)

// AccessPolicy is deployment configuration, never an API-managed resource.
// Tokens should be issued separately to administrators, agents and runners.
type AccessPolicy struct {
	Identities []AccessIdentity `json:"identities"`
}
type AccessIdentity struct {
	Name        string             `json:"name"`
	Token       string             `json:"token"`
	Permissions []AccessPermission `json:"permissions"`
}
type AccessPermission struct {
	Subresource   string   `json:"subresource,omitempty"`
	Kind          string   `json:"kind"`
	Methods       []string `json:"methods"`
	ResourceNames []string `json:"resourceNames,omitempty"`
}

func LoadAccessPolicy(path string) (*AccessPolicy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open API access policy: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("API policy must be a regular file that is not group/world writable")
	}
	var policy AccessPolicy
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("invalid API access policy")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("API access policy must contain one document")
	}
	seen := map[string]bool{}
	for _, identity := range policy.Identities {
		if identity.Name == "" || len(identity.Token) < 32 || seen[identity.Token] || strings.ContainsAny(identity.Token, " \t\r\n") {
			return nil, fmt.Errorf("API identities require unique tokens of at least 32 characters and a name")
		}
		seen[identity.Token] = true
		for _, permission := range identity.Permissions {
			if permission.Subresource != "" && permission.Subresource != "status" {
				return nil, fmt.Errorf("unsupported permission subresource")
			}
			known := permission.Kind == "*"
			for _, definition := range registry.Definitions {
				if definition.Kind == permission.Kind {
					known = true
				}
			}
			if !known {
				return nil, fmt.Errorf("unknown API permission kind %q", permission.Kind)
			}
			for _, method := range permission.Methods {
				switch method {
				case "GET", "POST", "PUT", "PATCH", "DELETE":
				default:
					return nil, fmt.Errorf("unsupported API permission method %q", method)
				}
			}
		}
	}
	if len(policy.Identities) == 0 {
		return nil, fmt.Errorf("API access policy must contain at least one identity")
	}
	return &policy, nil
}

// WithAccessPolicy rejects requests before validation or storage access. A nil
// policy denies API access; deliberately open development mode bypasses this
// wrapper in main. Only explicitly public GET routes bypass authentication.
func WithAccessPolicy(next http.Handler, policy *AccessPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		public := r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/openapi.json" || r.URL.Path == "/docs" || strings.HasPrefix(r.URL.Path, "/docs/") || strings.HasPrefix(r.URL.Path, "/ipxe/"))
		if public {
			next.ServeHTTP(w, r)
			return
		}
		authorization := r.Header.Get("Authorization")
		var identity *AccessIdentity
		if strings.HasPrefix(authorization, "Bearer ") && policy != nil {
			supplied := sha256.Sum256([]byte(strings.TrimPrefix(authorization, "Bearer ")))
			for i := range policy.Identities {
				expected := sha256.Sum256([]byte(policy.Identities[i].Token))
				if subtle.ConstantTimeCompare(supplied[:], expected[:]) == 1 {
					identity = &policy.Identities[i]
				}
			}
		}
		if identity == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "Unauthorized", "API bearer authentication is required")
			return
		}
		kind, name, subresource := "", "", ""
		for _, definition := range registry.Definitions {
			if r.URL.Path == definition.CollectionPath {
				kind = definition.Kind
				break
			}
			if strings.HasPrefix(r.URL.Path, definition.CollectionPath+"/") {
				kind, name = definition.Kind, strings.TrimPrefix(r.URL.Path, definition.CollectionPath+"/")
				if parts := strings.Split(name, "/"); len(parts) == 2 {
					name, subresource = parts[0], parts[1]
				} else if len(parts) > 2 {
					subresource = "invalid"
				}
				break
			}
		}
		// Collection-wide deletion of every kind is administrative only.
		if r.URL.Path == "/api/v1alpha1/resources" {
			kind = "*"
		}
		for _, permission := range identity.Permissions {
			if permission.Subresource != subresource && !(permission.Kind == "*" && permission.Subresource == "") {
				continue
			}
			if permission.Kind != "*" && permission.Kind != kind {
				continue
			}
			allowedName := len(permission.ResourceNames) == 0
			for _, resourceName := range permission.ResourceNames {
				if name != "" && name == resourceName {
					allowedName = true
				}
			}
			if !allowedName {
				continue
			}
			for _, method := range permission.Methods {
				if method == r.Method {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		writeError(w, http.StatusForbidden, "Forbidden", "This identity is not authorized for the resource operation")
	})
}
