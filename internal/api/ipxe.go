package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
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
	bootScript, err := serverBootScript(server)
	if err != nil {
		s.writeIPXEScript(w, fmt.Sprintf("Server %s: %s", server.Metadata.Name, err), "")
		return
	}
	s.writeIPXEScript(w, "Booting "+server.Metadata.Name, bootScript)
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
