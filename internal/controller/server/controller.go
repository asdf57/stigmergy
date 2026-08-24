package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

// ServerReconciler binds a user-named Server to exactly one discovered Machine.
// Both sides record UID-qualified references in status so names can be reused
// without accidentally inheriting an old binding.
type ServerReconciler struct {
	store store.Store
}

func NewReconciler(store store.Store) *ServerReconciler {
	return &ServerReconciler{store: store}
}

func (r *ServerReconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.ServerResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.releaseMachines(ctx, request.Name, "", nil)
	}
	if err != nil {
		return fmt.Errorf("get Server %q: %w", request.Name, err)
	}
	server, err := registry.ServerResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode Server %q: %w", request.Name, err)
	}

	location := canonicalLocation(server.Spec.MachineSelector.Location)
	machines, err := r.store.List(ctx, registry.MachineResource.Kind)
	if err != nil {
		return fmt.Errorf("list Machines: %w", err)
	}

	matches := make([]registry.Machine, 0, 1)
	for _, rawMachine := range machines.Items {
		machine, err := registry.MachineResource.Decode(rawMachine)
		if err != nil {
			return fmt.Errorf("decode Machine %q: %w", rawMachine.Metadata.Name, err)
		}
		reference, hasReference := resourceReference(machine.Status["serverRef"])
		staleSameNameReference := hasReference && reference.Name == server.Metadata.Name && reference.UID != server.Metadata.UID && canonicalLocation(machine.Spec.Location) != location
		if staleSameNameReference || (refMatches(machine.Status["serverRef"], server.Metadata.Name, server.Metadata.UID) && canonicalLocation(machine.Spec.Location) != location) {
			if err := r.releaseMachine(ctx, machine); err != nil {
				return err
			}
			machine.Status = cloneStatus(machine.Status)
			delete(machine.Status, "serverRef")
			machine.Status["phase"] = "Available"
		}
		if canonicalLocation(machine.Spec.Location) == location {
			matches = append(matches, machine)
		}
	}

	switch len(matches) {
	case 0:
		return r.updateServerBinding(ctx, server, nil, "Pending", "False", "NoMatchingMachine", "No discovered Machine matches the requested LLDP location")
	case 1:
		// Continue below.
	default:
		return r.updateServerBinding(ctx, server, nil, "Conflict", "False", "AmbiguousLocation", fmt.Sprintf("%d Machines match the requested LLDP location", len(matches)))
	}

	machine := matches[0]
	if reference, ok := resourceReference(machine.Status["serverRef"]); ok && (reference.Name != server.Metadata.Name || reference.UID != server.Metadata.UID) {
		active, err := r.serverReferenceActive(ctx, reference)
		if err != nil {
			return err
		}
		if active {
			return r.updateServerBinding(ctx, server, nil, "Conflict", "False", "MachineAlreadyBound", fmt.Sprintf("Machine %q is bound to Server %q", machine.Metadata.Name, reference.Name))
		}
	}
	if !refMatches(machine.Status["serverRef"], server.Metadata.Name, server.Metadata.UID) {
		if err := r.bindMachine(ctx, machine, server); err != nil {
			return err
		}
	}
	return r.updateServerBinding(ctx, server, &machine, "Bound", "True", "MachineBound", fmt.Sprintf("Bound to Machine %q", machine.Metadata.Name))
}

func (r *ServerReconciler) serverReferenceActive(ctx context.Context, reference reference) (bool, error) {
	raw, err := r.store.Get(ctx, registry.ServerResource.Kind, reference.Name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get referenced Server %q: %w", reference.Name, err)
	}
	return raw.Metadata.UID == reference.UID, nil
}

// RequestsForMachine conservatively reconciles every Server when Machine
// availability or location changes. The resource set is intentionally small;
// an indexed mapper can replace this without changing binding semantics.
func (r *ServerReconciler) RequestsForMachine(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	servers, err := r.store.List(ctx, registry.ServerResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list Servers for Machine event: %w", err)
	}
	requests := make([]controller.Request, 0, len(servers.Items))
	for _, server := range servers.Items {
		requests = append(requests, controller.Request{Kind: registry.ServerResource.Kind, Name: server.Metadata.Name})
	}
	return requests, nil
}

func (r *ServerReconciler) bindMachine(ctx context.Context, machine registry.Machine, server registry.Server) error {
	status := cloneStatus(machine.Status)
	status["serverRef"] = map[string]any{"name": server.Metadata.Name, "uid": server.Metadata.UID}
	status["phase"] = "Bound"
	return r.writeMachineStatus(ctx, machine, status)
}

func (r *ServerReconciler) releaseMachine(ctx context.Context, machine registry.Machine) error {
	status := cloneStatus(machine.Status)
	delete(status, "serverRef")
	status["phase"] = "Available"
	return r.writeMachineStatus(ctx, machine, status)
}

func (r *ServerReconciler) releaseMachines(ctx context.Context, serverName, serverUID string, except *apigen.MachineLocation) error {
	machines, err := r.store.List(ctx, registry.MachineResource.Kind)
	if err != nil {
		return fmt.Errorf("list Machines while releasing Server %q: %w", serverName, err)
	}
	for _, raw := range machines.Items {
		machine, err := registry.MachineResource.Decode(raw)
		if err != nil {
			return fmt.Errorf("decode Machine %q: %w", raw.Metadata.Name, err)
		}
		if !refMatches(machine.Status["serverRef"], serverName, serverUID) {
			continue
		}
		if except != nil && canonicalLocation(machine.Spec.Location) == canonicalLocation(*except) {
			continue
		}
		if err := r.releaseMachine(ctx, machine); err != nil {
			return err
		}
	}
	return nil
}

func (r *ServerReconciler) writeMachineStatus(ctx context.Context, machine registry.Machine, status map[string]any) error {
	if resource.EqualJSON(machine.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(machine.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse Machine %q resource version: %w", machine.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, machine.Kind, machine.Metadata.Name, status, revision); err != nil {
		return fmt.Errorf("update Machine %q binding status: %w", machine.Metadata.Name, err)
	}
	slog.Info("updated Machine binding", "machine", machine.Metadata.Name)
	return nil
}

func (r *ServerReconciler) updateServerBinding(ctx context.Context, server registry.Server, machine *registry.Machine, phase, conditionStatus, reason, message string) error {
	status := cloneStatus(server.Status)
	if machine == nil {
		delete(status, "machineRef")
	} else {
		status["machineRef"] = map[string]any{"name": machine.Metadata.Name, "uid": machine.Metadata.UID}
	}
	if current, _ := status["phase"].(string); current == "" || current == "Pending" || current == "Conflict" || phase != "Bound" {
		status["phase"] = phase
	}
	status["conditions"] = upsertCondition(status["conditions"], map[string]any{
		"type":               "MachineBound",
		"status":             conditionStatus,
		"reason":             reason,
		"message":            message,
		"observedGeneration": server.Metadata.Generation,
	})
	applyManagementNetworkStatus(status, server, machine)
	if server.Spec.HostName != nil {
		fqdn := *server.Spec.HostName
		if server.Spec.DomainName != nil && *server.Spec.DomainName != "" {
			fqdn += "." + *server.Spec.DomainName
		}
		status["fqdn"] = fqdn
	}
	if resource.EqualJSON(server.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(server.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse Server %q resource version: %w", server.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, server.Kind, server.Metadata.Name, status, revision); err != nil {
		return fmt.Errorf("update Server %q binding status: %w", server.Metadata.Name, err)
	}
	return nil
}

func applyManagementNetworkStatus(status map[string]any, server registry.Server, machine *registry.Machine) {
	if server.Spec.Networking == nil || server.Spec.Networking.Management == nil {
		delete(status, "networking")
		status["conditions"] = removeCondition(status["conditions"], "ManagementAddressReady")
		return
	}

	condition := map[string]any{
		"type":               "ManagementAddressReady",
		"status":             "False",
		"observedGeneration": server.Metadata.Generation,
	}
	fail := func(reason, message string) {
		delete(status, "networking")
		condition["reason"] = reason
		condition["message"] = message
		status["conditions"] = upsertCondition(status["conditions"], condition)
	}
	if machine == nil {
		fail("MachineNotBound", "Management address cannot be resolved until the Server is bound to a Machine")
		return
	}

	management := server.Spec.Networking.Management
	prefix, err := netip.ParsePrefix(management.AddressSelector.Subnet)
	if err != nil {
		fail("InvalidSubnet", fmt.Sprintf("Management address subnet %q is invalid", management.AddressSelector.Subnet))
		return
	}
	inventory, err := decodeInventory(machine.Status["inventory"])
	if err != nil {
		fail("InventoryNotAvailable", "The bound Machine does not have usable inventory")
		return
	}

	interfaceName := interfaceAtLocation(inventory.LLDPInfo, machine.Spec.Location)
	if interfaceName == "" {
		fail("ManagementInterfaceNotFound", "No reported interface is attached at the Machine's LLDP location")
		return
	}
	var selectedInterface *apigen.MachineReportNetworkInterface
	for index := range inventory.Interfaces {
		if inventory.Interfaces[index].Name == interfaceName {
			selectedInterface = &inventory.Interfaces[index]
			break
		}
	}
	if selectedInterface == nil {
		fail("ManagementInterfaceNotFound", fmt.Sprintf("LLDP identified interface %q, but it is absent from interface inventory", interfaceName))
		return
	}

	family := string(management.AddressSelector.Family)
	matches := make([]apigen.MachineReportNetworkAddress, 0, 1)
	for _, address := range selectedInterface.Addresses {
		parsed, err := netip.ParseAddr(address.Address)
		if err != nil || string(address.Family) != family || !prefix.Contains(parsed) {
			continue
		}
		matches = append(matches, address)
	}
	if len(matches) == 0 {
		fail("ManagementAddressNotFound", fmt.Sprintf("Interface %q has no %s address in %s", interfaceName, family, prefix))
		return
	}
	if len(matches) > 1 {
		fail("ManagementAddressAmbiguous", fmt.Sprintf("Interface %q has %d %s addresses in %s", interfaceName, len(matches), family, prefix))
		return
	}

	match := matches[0]
	status["networking"] = map[string]any{
		"management": map[string]any{
			"interface": map[string]any{"name": selectedInterface.Name, "mac": selectedInterface.Mac},
			"address": map[string]any{
				"address":      match.Address,
				"family":       string(match.Family),
				"prefixLength": match.PrefixLength,
			},
			"reason": "AttachedAtMachineLocation",
		},
	}
	condition["status"] = "True"
	condition["reason"] = "ManagementAddressResolved"
	condition["message"] = fmt.Sprintf("Resolved %s on interface %s", match.Address, selectedInterface.Name)
	status["conditions"] = upsertCondition(status["conditions"], condition)
}

func decodeInventory(value any) (apigen.MachineReportSpec, error) {
	var inventory apigen.MachineReportSpec
	if value == nil {
		return inventory, errors.New("inventory is absent")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return inventory, err
	}
	if err := json.Unmarshal(encoded, &inventory); err != nil {
		return inventory, err
	}
	return inventory, nil
}

func interfaceAtLocation(groups []apigen.MachineReportLLDPInterfaceGroup, location apigen.MachineLocation) string {
	location = canonicalLocation(location)
	for _, group := range groups {
		for _, iface := range group.Interface {
			portMatches := false
			for _, port := range iface.Ports {
				for _, id := range port.IDs {
					if strings.TrimSpace(id.Value) == location.LldpPort {
						portMatches = true
					}
				}
			}
			chassisMatches := false
			for _, chassis := range iface.Chassis {
				for _, id := range chassis.IDs {
					if strings.EqualFold(id.Type, "mac") && strings.EqualFold(strings.TrimSpace(id.Value), location.SwitchMac) {
						chassisMatches = true
					}
				}
			}
			if portMatches && chassisMatches {
				return iface.Name
			}
		}
	}
	return ""
}

type reference struct {
	Name string
	UID  string
}

func resourceReference(value any) (reference, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return reference{}, false
	}
	name, nameOK := object["name"].(string)
	uid, uidOK := object["uid"].(string)
	return reference{Name: name, UID: uid}, nameOK && uidOK && name != "" && uid != ""
}

func refMatches(value any, name, uid string) bool {
	reference, ok := resourceReference(value)
	if !ok || reference.Name != name {
		return false
	}
	// Delete events only retain the object's name, so an empty expected UID
	// deliberately matches any historical instance of that name.
	return uid == "" || reference.UID == uid
}

func cloneStatus(status map[string]any) map[string]any {
	cloned := make(map[string]any, len(status)+2)
	for key, value := range status {
		cloned[key] = value
	}
	return cloned
}

func upsertCondition(value any, desired map[string]any) []any {
	conditions, _ := value.([]any)
	updated := make([]any, 0, len(conditions)+1)
	for _, condition := range conditions {
		object, ok := condition.(map[string]any)
		if ok && object["type"] == desired["type"] {
			continue
		}
		updated = append(updated, condition)
	}
	return append(updated, desired)
}

func removeCondition(value any, conditionType string) []any {
	conditions, _ := value.([]any)
	updated := make([]any, 0, len(conditions))
	for _, condition := range conditions {
		object, ok := condition.(map[string]any)
		if ok && object["type"] == conditionType {
			continue
		}
		updated = append(updated, condition)
	}
	return updated
}

func canonicalLocation(location apigen.MachineLocation) apigen.MachineLocation {
	return apigen.MachineLocation{
		LldpPort:  strings.TrimSpace(location.LldpPort),
		SwitchMac: strings.ToLower(strings.TrimSpace(location.SwitchMac)),
	}
}
