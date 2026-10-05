package isobuild

import (
	"context"
	"encoding/json"
	"fmt"
	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Manifest struct {
	ISOUID            string               `json:"isoUid"`
	InputRevision     string               `json:"inputRevision"`
	InputContent      string               `json:"inputContent"`
	TrustBundleDigest string               `json:"trustBundleDigest"`
	BuildID           string               `json:"buildId"`
	BuildStartedAt    time.Time            `json:"buildStartedAt"`
	SourceRevisions   map[string]string    `json:"sourceRevisions"`
	Artifacts         []apigen.ISOArtifact `json:"artifacts"`
}
type ManifestReader interface {
	Latest(context.Context, string, string, string) (*Manifest, error)
}
type CopypartyReader struct {
	BaseURL string
	Client  *http.Client
}

var manifestName = regexp.MustCompile(`^[a-f0-9-]{36}\.json$`)
var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (r *CopypartyReader) get(ctx context.Context, address string, value any) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return false, err
	}
	response, err := r.Client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return false, nil
	}
	if response.StatusCode != 200 {
		return false, fmt.Errorf("artifact discovery returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return false, err
	}
	if len(data) > 1<<20 {
		return false, fmt.Errorf("artifact discovery response is too large")
	}
	err = json.Unmarshal(data, value)
	return true, err
}

// Read immutable per-build manifests. Prefer the newest start time among builds
// matching the current input content. A late old build cannot replace new input.
// Source-only rebuilds use this same ordering and retain exact Git provenance.
func (r *CopypartyReader) Latest(ctx context.Context, uid, input, digest string) (*Manifest, error) {
	base := strings.TrimRight(r.BaseURL, "/") + "/iso-resources/" + url.PathEscape(uid) + "/"
	directory := base + "manifests/"
	var listing struct {
		Files []struct {
			Href string `json:"href"`
		} `json:"files"`
	}
	found, err := r.get(ctx, directory+"?ls", &listing)
	if !found || err != nil {
		return nil, err
	}
	if len(listing.Files) > 1000 {
		return nil, fmt.Errorf("manifest retention is required: more than 1000 builds")
	}
	var latest *Manifest
	for _, file := range listing.Files {
		// Never follow an arbitrary listing URL, redirect, path or origin.
		if !manifestName.MatchString(file.Href) {
			continue
		}
		var candidate Manifest
		found, err := r.get(ctx, directory+file.Href, &candidate)
		if err != nil {
			return nil, err
		}
		if !found || candidate.ISOUID != uid || candidate.InputContent != input || candidate.TrustBundleDigest != digest {
			continue
		}
		if candidate.BuildStartedAt.IsZero() || candidate.BuildStartedAt.After(time.Now().Add(5*time.Minute)) || candidate.InputRevision == "" || candidate.BuildID+".json" != file.Href || candidate.SourceRevisions["builder"] == "" || candidate.SourceRevisions["homelabd"] == "" {
			return nil, fmt.Errorf("invalid matching build manifest")
		}
		seen := map[apigen.ISOArtifactType]bool{}
		for _, artifact := range candidate.Artifacts {
			if seen[artifact.Type] || !hash.MatchString(artifact.Sha256) || !strings.HasPrefix(artifact.Url, base+"builds/"+candidate.BuildID+"/") || strings.ContainsAny(artifact.Url, "\r\n \t") {
				return nil, fmt.Errorf("invalid artifact in build manifest")
			}
			parsed, err := url.Parse(artifact.Url)
			if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(parsed.Path, "/../") {
				return nil, fmt.Errorf("unsafe artifact URL")
			}
			seen[artifact.Type] = true
		}
		for _, kind := range []apigen.ISOArtifactType{"iso", "kernel", "initrd", "rootfs"} {
			if !seen[kind] {
				return nil, fmt.Errorf("build manifest is missing %s", kind)
			}
		}
		if latest == nil || candidate.BuildStartedAt.After(latest.BuildStartedAt) || candidate.BuildStartedAt.Equal(latest.BuildStartedAt) && candidate.BuildID > latest.BuildID {
			copy := candidate
			latest = &copy
		}
	}
	return latest, nil
}
