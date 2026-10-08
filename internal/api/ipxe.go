package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/resource"
)

func (s *Server) getIPXEBoot(w http.ResponseWriter, r *http.Request) {
	requestedMAC, err := canonicalMAC(r.PathValue("mac"))
	if err != nil {
		s.writeIPXEScript(w, "Invalid boot MAC", "")
		return
	}

	resources, err := s.store.List(r.Context(), registry.MachineResource.Kind)
	if err != nil {
		s.logger.Error("list Machines for iPXE", "error", err)
		s.writeIPXEScript(w, "Unable to resolve boot configuration", "")
		return
	}

	matches := make([]registry.Machine, 0, 1)
	for _, raw := range resources.Items {
		machine, err := registry.MachineResource.Decode(raw)
		if err != nil {
			s.logger.Error("decode Machine for iPXE", "name", raw.Metadata.Name, "error", err)
			continue
		}
		if machineBootMACMatches(machine, requestedMAC) {
			matches = append(matches, machine)
		}
	}

	if len(matches) == 0 {
		s.serveDiscoveryBoot(w, r, "No discovered Machine matches MAC "+requestedMAC)
		return
	}
	if len(matches) > 1 {
		s.writeIPXEScript(w, "Multiple Servers match MAC "+requestedMAC, "")
		return
	}

	machine := matches[0]
	if machine.Status == nil || machine.Status.ServerRef == nil {
		s.serveDiscoveryBoot(w, r, "Machine at this MAC is not bound to a Server")
		return
	}
	reference := machine.Status.ServerRef
	rawServer, err := s.store.Get(r.Context(), registry.ServerResource.Kind, reference.Name)
	if err != nil {
		s.writeIPXEScript(w, "Bound Server is unavailable", "")
		return
	}
	if rawServer.Metadata.UID != reference.Uid {
		s.writeIPXEScript(w, "Machine has a stale Server binding", "")
		return
	}
	server, err := registry.ServerResource.Decode(rawServer)
	if err != nil {
		s.writeIPXEScript(w, "Bound Server is invalid", "")
		return
	}
	if server.Metadata.DeletionTimestamp != nil || server.Status == nil || server.Status.MachineRef == nil || server.Status.MachineRef.Uid != machine.Metadata.UID || server.Status.MachineRef.Name != machine.Metadata.Name {
		s.writeIPXEScript(w, "Server has a stale Machine binding", "")
		return
	}
	if server.Spec.Provisioning == nil || !server.Spec.Provisioning.Enabled || server.Spec.Reconciliation != nil && server.Spec.Reconciliation.Paused {
		s.writeIPXEScript(w, "No eligible live provisioning request", "")
		return
	}
	if reservation := server.Status.Provisioning; reservation != nil && reservation.ActiveRunRef != nil {
		raw, err := s.store.Get(r.Context(), "ProvisioningRun", reservation.ActiveRunRef.Name)
		if err != nil || raw.Metadata.UID != reservation.ActiveRunRef.Uid || raw.Metadata.DeletionTimestamp != nil {
			s.writeIPXEScript(w, "Reserved ProvisioningRun is unavailable", "")
			return
		}
		run, err := registry.ProvisioningRunResource.Decode(raw)
		if err != nil || run.Spec.ServerRef.Uid != server.Metadata.UID || run.Spec.MachineRef.Uid != machine.Metadata.UID || run.Status == nil {
			s.writeIPXEScript(w, "ProvisioningRun binding changed", "")
			return
		}
		p := run.Status
		if p.Phase != nil && *p.Phase == "Pending" {
			s.serveISOBoot(w, r, server)
			return
		}
		if p.Phase == nil || (*p.Phase != "PreparingBoot" && *p.Phase != "AwaitingLive" && *p.Phase != "Installing") || p.Snapshot == nil {
			s.writeIPXEScript(w, "This attempt does not permit live boot", "")
			return
		}
		if p.Snapshot.BootMAC != requestedMAC {
			s.writeIPXEScript(w, "MAC does not match pinned provisioning interface", "")
			return
		}
		s.servePinnedBoot(w, r, server, p.Snapshot)
		return
	}
	if server.Spec.Boot == nil || server.Spec.OperatingSystem == nil || server.Status.Provisioning != nil && server.Status.Provisioning.Provisioned {
		s.writeIPXEScript(w, "Initial provisioning configuration is incomplete or already observed", "")
		return
	}
	s.serveISOBoot(w, r, server)
}

func (s *Server) serveDiscoveryBoot(w http.ResponseWriter, r *http.Request, message string) {
	if s.discoveryISO == "" {
		s.writeIPXEScript(w, message, "")
		return
	}
	raw, err := s.store.Get(r.Context(), registry.ISOResource.Kind, s.discoveryISO)
	if err != nil {
		s.writeIPXEScript(w, "Discovery ISO is unavailable", "")
		return
	}
	image, err := registry.ISOResource.Decode(raw)
	if err != nil || image.Status == nil || image.Status.AuthorityRef == nil {
		s.writeIPXEScript(w, "Discovery ISO is not Ready", "")
		return
	}
	host := registry.NewServer(resource.Metadata{}, apigen.ServerSpec{Boot: &apigen.ServerBootSpec{IsoRef: apigen.ServerDependencyReference{Name: s.discoveryISO}}, SshCertificateAuthorityRef: &apigen.ServerDependencyReference{Name: image.Spec.SshCertificateAuthorityRef.Name, Uid: image.Spec.SshCertificateAuthorityRef.Uid}})
	host.Status = &apigen.ServerStatus{BootISORef: &apigen.ResourceReference{Name: s.discoveryISO, Uid: raw.Metadata.UID}, SshTrust: &apigen.ServerSSHTrustStatus{AuthorityRef: *image.Status.AuthorityRef}}
	s.serveISOBoot(w, r, host)
}

func (s *Server) servePinnedBoot(w http.ResponseWriter, r *http.Request, server registry.Server, p *apigen.ProvisioningRunSnapshot) {
	if p.ServerUID != server.Metadata.UID || p.MachineRef != *server.Status.MachineRef || p.AuthorityRef.Uid == "" {
		s.writeIPXEScript(w, "Pinned boot identity changed", "")
		return
	}
	for kind, ref := range map[string]apigen.ResourceReference{"ISO": p.IsoRef, "SSHCertificateAuthority": p.AuthorityRef, "SSHKeyPair": p.KeyPairRef} {
		raw, err := s.store.Get(r.Context(), kind, ref.Name)
		if err != nil || raw.Metadata.UID != ref.Uid || raw.Metadata.DeletionTimestamp != nil {
			s.writeIPXEScript(w, "Pinned boot dependency was removed or replaced", "")
			return
		}
	}
	s.renderISOBoot(w, string(p.Distribution), p.Artifacts)
}

func (s *Server) serveISOBoot(w http.ResponseWriter, r *http.Request, server registry.Server) {
	fail := func(message string) { s.writeIPXEScript(w, message, "") }
	ref := server.Spec.Boot.IsoRef
	raw, err := s.store.Get(r.Context(), registry.ISOResource.Kind, ref.Name)
	if err != nil || raw.Metadata.DeletionTimestamp != nil {
		fail("Selected ISO is unavailable")
		return
	}
	if server.Status == nil || server.Status.BootISORef == nil || server.Status.BootISORef.Name != ref.Name || server.Status.BootISORef.Uid != raw.Metadata.UID || ref.Uid != nil && *ref.Uid != raw.Metadata.UID {
		fail("Selected ISO identity has not been bound")
		return
	}
	image, err := registry.ISOResource.Decode(raw)
	if err != nil || image.Status == nil || image.Status.Phase == nil || *image.Status.Phase != "Ready" || image.Status.ObservedGeneration == nil || *image.Status.ObservedGeneration != image.Metadata.Generation || image.Status.Artifacts == nil {
		fail("Selected ISO is awaiting a completed build")
		return
	}
	if server.Spec.SshCertificateAuthorityRef == nil || server.Spec.SshCertificateAuthorityRef.Name != image.Spec.SshCertificateAuthorityRef.Name || server.Status.SshTrust == nil || image.Status.AuthorityRef == nil || server.Status.SshTrust.AuthorityRef.Uid != image.Status.AuthorityRef.Uid {
		fail("Server and ISO SSH authorities do not match")
		return
	}
	authorityRaw, err := s.store.Get(r.Context(), registry.SSHCertificateAuthorityResource.Kind, server.Spec.SshCertificateAuthorityRef.Name)
	if err != nil {
		fail("Selected authority is unavailable")
		return
	}
	authority, err := registry.SSHCertificateAuthorityResource.Decode(authorityRaw)
	if err != nil || authority.Metadata.DeletionTimestamp != nil || authority.Metadata.UID != image.Status.AuthorityRef.Uid || authority.Status == nil || authority.Status.Phase == nil || *authority.Status.Phase != "Ready" || authority.Status.ObservedGeneration == nil || *authority.Status.ObservedGeneration != authority.Metadata.Generation || authority.Status.TrustBundleDigest == nil || image.Status.DesiredTrustBundleDigest == nil || *authority.Status.TrustBundleDigest != *image.Status.DesiredTrustBundleDigest || (server.Spec.SshCertificateAuthorityRef.Uid != nil && *server.Spec.SshCertificateAuthorityRef.Uid != authority.Metadata.UID) || (image.Spec.SshCertificateAuthorityRef.Uid != nil && *image.Spec.SshCertificateAuthorityRef.Uid != authority.Metadata.UID) {
		fail("Selected image does not match the current authority identity and trust")
		return
	}
	s.renderISOBoot(w, string(image.Spec.Distribution), *image.Status.Artifacts)
}

func (s *Server) renderISOBoot(w http.ResponseWriter, distribution string, values []apigen.ISOArtifact) {
	fail := func(message string) { s.writeIPXEScript(w, message, "") }
	artifacts := map[string]string{}
	for _, artifact := range values {
		parsed, err := url.Parse(artifact.Url)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || strings.ContainsAny(artifact.Url, " \t\r\n$") || parsed.RawQuery != "" || parsed.Fragment != "" {
			fail("Invalid ISO artifact URL")
			return
		}
		if artifacts[string(artifact.Type)] != "" {
			fail("Duplicate ISO artifact type")
			return
		}
		artifacts[string(artifact.Type)] = artifact.Url
	}
	for _, kind := range []string{"kernel", "initrd", "rootfs"} {
		if artifacts[kind] == "" {
			fail("Selected ISO has incomplete netboot artifacts")
			return
		}
	}
	arguments := ""
	switch distribution {
	case "debian":
		// live-boot treats ip=dhcp as STATICIP and writes "nameserver dhcp".
		// Fetch already requests DHCP; retain the boot NIC without a static override.
		arguments = "boot=live components BOOTIF=01-${netX/mac} fetch=" + artifacts["rootfs"]
	case "arch":
		const suffix = "arch/x86_64/airootfs.sfs"
		if !strings.HasSuffix(artifacts["rootfs"], suffix) {
			fail("Invalid Arch netboot rootfs path")
			return
		}
		arguments = "archisobasedir=arch archiso_http_srv=" + strings.TrimSuffix(artifacts["rootfs"], suffix) + " ip=dhcp net.ifnames=0 BOOTIF=01-${netX/mac}"
	default:
		fail("Unsupported ISO netboot recipe")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "#!ipxe\nkernel %s %s initrd=initrd.img\ninitrd --name initrd.img %s\nboot\n", artifacts["kernel"], arguments, artifacts["initrd"])
}

func machineBootMACMatches(machine registry.Machine, requested string) bool {
	if machine.Status == nil || machine.Status.Inventory == nil {
		return false
	}
	for _, networkInterface := range machine.Status.Inventory.Interfaces {
		candidate, err := canonicalMAC(networkInterface.Mac)
		if err == nil && candidate == requested {
			return true
		}
	}
	return false
}

func canonicalMAC(value string) (string, error) {
	parsed, err := net.ParseMAC(value)
	if err != nil {
		return "", err
	}
	return strings.ToLower(parsed.String()), nil
}

func (s *Server) writeIPXEScript(w http.ResponseWriter, message, bootScript string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "#!ipxe")
	_, _ = fmt.Fprintln(w, "echo "+message)
	if bootScript == "" {
		_, _ = fmt.Fprintln(w, "sleep 10")
		_, _ = fmt.Fprintln(w, "exit 1")
		return
	}
	_, _ = fmt.Fprintln(w, "chain "+bootScript)
}
