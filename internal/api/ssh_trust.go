package api

import (
	"context"
	"fmt"

	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
)

// Domain validation is separate from generic status transport/authorization.
// Daemon observations cannot attest management convergence.
func (s *Server) validateServerTrustStatus(ctx context.Context, raw resource.Resource, merged map[string]any) error {
	digest, ok := merged["installedSSHTrustBundleDigest"].(string)
	if !ok || digest == "" {
		return fmt.Errorf("installed trust must be a verified digest")
	}
	server, err := registry.ServerResource.Decode(raw)
	if err != nil {
		return err
	}
	if server.Status == nil || server.Status.SshTrust == nil || server.Status.DesiredSSHTrustBundleDigest == nil || *server.Status.DesiredSSHTrustBundleDigest != digest {
		return fmt.Errorf("desired trust changed; verify again")
	}
	if server.Spec.SshCertificateAuthorityRef == nil || server.Spec.SshCertificateAuthorityRef.Name != server.Status.SshTrust.AuthorityRef.Name {
		return fmt.Errorf("desired authority changed")
	}
	authorityRaw, err := s.store.Get(ctx, registry.SSHCertificateAuthorityResource.Kind, server.Spec.SshCertificateAuthorityRef.Name)
	if err != nil {
		return err
	}
	authority, err := registry.SSHCertificateAuthorityResource.Decode(authorityRaw)
	if err != nil {
		return err
	}
	uid := server.Status.SshTrust.AuthorityRef.Uid
	if authority.Metadata.DeletionTimestamp != nil || authority.Metadata.UID != uid || authority.Status == nil || authority.Status.Phase == nil || *authority.Status.Phase != "Ready" || authority.Status.ObservedGeneration == nil || *authority.Status.ObservedGeneration != authority.Metadata.Generation || authority.Status.TrustBundleDigest == nil || *authority.Status.TrustBundleDigest != digest || (server.Spec.SshCertificateAuthorityRef.Uid != nil && *server.Spec.SshCertificateAuthorityRef.Uid != uid) {
		return fmt.Errorf("authority identity or trust changed; verify again")
	}
	return nil
}
