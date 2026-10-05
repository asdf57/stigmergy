package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/asdf57/stigmergy/internal/api/registry"
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
		s.writeIPXEScript(w, "No discovered Machine matches MAC "+requestedMAC, "")
		return
	}
	if len(matches) > 1 {
		s.writeIPXEScript(w, "Multiple Servers match MAC "+requestedMAC, "")
		return
	}

	machine := matches[0]
	if machine.Status == nil || machine.Status.ServerRef == nil {
		s.writeIPXEScript(w, "Machine at this MAC is not bound to a Server", "")
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
	if server.Spec.Boot != nil {
		s.serveISOBoot(w, r, server)
		return
	}
	bootScript, err := serverBootScript(server)
	if err != nil {
		s.writeIPXEScript(w, fmt.Sprintf("Server %s: %s", server.Metadata.Name, err), "")
		return
	}
	s.writeIPXEScript(w, "Booting "+server.Metadata.Name, bootScript)
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
	artifacts := map[string]string{}
	for _, artifact := range *image.Status.Artifacts {
		parsed, err := url.Parse(artifact.Url)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || strings.ContainsAny(artifact.Url, " \t\r\n$") || parsed.RawQuery != "" || parsed.Fragment != "" {
			fail("Invalid ISO artifact URL")
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
	switch image.Spec.Distribution {
	case "debian":
		arguments = "boot=live components ip=dhcp fetch=" + artifacts["rootfs"]
	case "arch":
		const suffix = "arch/x86_64/airootfs.sfs"
		if !strings.HasSuffix(artifacts["rootfs"], suffix) {
			fail("Invalid Arch netboot rootfs path")
			return
		}
		arguments = "archisobasedir=arch archiso_http_srv=" + strings.TrimSuffix(artifacts["rootfs"], suffix) + " ip=dhcp"
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

func serverBootScript(server registry.Server) (string, error) {
	if server.Spec.OperatingSystem == nil {
		return "", errors.New("spec.operatingSystem is not configured")
	}
	os := server.Spec.OperatingSystem

	distribution := strings.ToLower(strings.ReplaceAll(os.Distribution, "_", "-"))
	switch distribution {
	case "arch", "archlinux":
		return "/arch_boot.ipxe", nil
	case "debian", "debian-trixie":
		return "/debian_boot.ipxe", nil
	default:
		return "", fmt.Errorf("distribution %q is not supported by the PXE images", os.Distribution)
	}
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
