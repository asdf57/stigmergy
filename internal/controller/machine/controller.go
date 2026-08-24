package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type MachineReportReconciler struct {
	store store.Store
}

func NewReconciler(store store.Store) *MachineReportReconciler {
	return &MachineReportReconciler{store: store}
}

// NewMachineReportReconciler is kept for callers migrating to NewReconciler.
func NewMachineReportReconciler(store store.Store) *MachineReportReconciler {
	return NewReconciler(store)
}

func (r *MachineReportReconciler) Reconcile(ctx context.Context, event controller.Request) error {
	rsrc, err := r.store.Get(ctx, registry.MachineReportResource.Kind, event.Name)
	if errors.Is(err, store.ErrNotFound) {
		slog.Info("MachineReport not found, skipping", "name", event.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get MachineReport %q: %w", event.Name, err)
	}

	report, err := registry.MachineReportResource.Decode(rsrc)
	if err != nil {
		return fmt.Errorf("decode MachineReport %q: %w", event.Name, err)
	}
	reportRevision, err := strconv.ParseInt(report.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse MachineReport %q resource version: %w", event.Name, err)
	}

	location := canonicalLocation(machineLocation(report.Spec.LLDPInfo))
	if location.LldpPort == "" || location.SwitchMac == "" {
		return fmt.Errorf("MachineReport %q does not contain a complete LLDP location", report.Metadata.Name)
	}
	slog.Debug("reconciling MachineReport", "name", report.Metadata.Name, "lldpPort", location.LldpPort, "switchMac", location.SwitchMac)

	machine, err := r.machineAtLocation(ctx, location)
	if err != nil {
		return err
	}
	if machine == nil {
		created, err := r.createMachine(ctx, location)
		if err != nil {
			return err
		}
		machine = &created
	}

	inventory, err := reportStatus(report.Spec)
	if err != nil {
		return fmt.Errorf("build Machine status from report %q: %w", report.Metadata.Name, err)
	}
	if observedAt, ok := machineObservedAt(machine.Status); ok && observedAt.After(report.Spec.ObservedAt) {
		slog.Info("discarding stale MachineReport", "name", report.Metadata.Name, "observedAt", report.Spec.ObservedAt, "machineObservedAt", observedAt)
		return r.consumeReport(ctx, report, reportRevision)
	}
	status := mergeInventoryStatus(machine.Status, inventory)
	if !resource.EqualJSON(machine.Status, status) {
		revision, err := strconv.ParseInt(machine.Metadata.ResourceVersion, 10, 64)
		if err != nil {
			return fmt.Errorf("parse Machine %q resource version: %w", machine.Metadata.Name, err)
		}
		updated, err := r.store.UpdateStatus(ctx, machine.Kind, machine.Metadata.Name, status, revision)
		if err != nil {
			return fmt.Errorf("update Machine %q status: %w", machine.Metadata.Name, err)
		}
		slog.Info("updated Machine status", "name", updated.Metadata.Name, "uid", updated.Metadata.UID)
	}
	return r.consumeReport(ctx, report, reportRevision)
}

func (r *MachineReportReconciler) machineAtLocation(ctx context.Context, location apigen.MachineLocation) (*registry.Machine, error) {
	machines, err := r.store.List(ctx, registry.MachineResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list Machines: %w", err)
	}

	var match *registry.Machine
	for index := range machines.Items {
		machine, err := registry.MachineResource.Decode(machines.Items[index])
		if err != nil {
			return nil, fmt.Errorf("decode Machine %q: %w", machines.Items[index].Metadata.Name, err)
		}
		if canonicalLocation(machine.Spec.Location) != location {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("LLDP location %s/%s is claimed by both Machine %q and %q", location.SwitchMac, location.LldpPort, match.Metadata.Name, machine.Metadata.Name)
		}
		match = &machine
	}
	return match, nil
}

func (r *MachineReportReconciler) createMachine(ctx context.Context, location apigen.MachineLocation) (registry.Machine, error) {
	name := machineNameForLocation(location)
	desired := registry.NewMachine(resource.Metadata{Name: name}, apigen.MachineSpec{Location: location})
	candidate, err := desired.Encode()
	if err != nil {
		return registry.Machine{}, fmt.Errorf("encode Machine %q: %w", name, err)
	}
	if err := resource.ValidateCreate(candidate); err != nil {
		return registry.Machine{}, fmt.Errorf("validate Machine %q: %w", name, err)
	}

	created, err := r.store.Create(ctx, candidate)
	if errors.Is(err, store.ErrConflict) {
		created, err = r.store.Get(ctx, registry.MachineResource.Kind, name)
	}
	if err != nil {
		return registry.Machine{}, fmt.Errorf("create Machine %q: %w", name, err)
	}
	machine, err := registry.MachineResource.Decode(created)
	if err != nil {
		return registry.Machine{}, fmt.Errorf("decode created Machine %q: %w", name, err)
	}
	if canonicalLocation(machine.Spec.Location) != location {
		return registry.Machine{}, fmt.Errorf("generated Machine name %q is already used for a different location", name)
	}
	slog.Info("created Machine", "name", machine.Metadata.Name, "uid", machine.Metadata.UID)
	return machine, nil
}

func (r *MachineReportReconciler) consumeReport(ctx context.Context, report registry.MachineReport, revision int64) error {
	if err := r.store.Delete(ctx, report.Kind, report.Metadata.Name, revision); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("consume MachineReport %q: %w", report.Metadata.Name, err)
	}
	slog.Info("consumed MachineReport", "name", report.Metadata.Name, "resourceVersion", revision)
	return nil
}

func reportStatus(spec apigen.MachineReportSpec) (map[string]any, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var status map[string]any
	if err := json.Unmarshal(encoded, &status); err != nil {
		return nil, err
	}
	return status, nil
}

func machineObservedAt(status map[string]any) (time.Time, bool) {
	inventory, ok := status["inventory"].(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	value, ok := inventory["observed_at"].(string)
	if !ok {
		return time.Time{}, false
	}
	observedAt, err := time.Parse(time.RFC3339Nano, value)
	return observedAt, err == nil
}

func mergeInventoryStatus(status, inventory map[string]any) map[string]any {
	// Inventory is owned by this reconciler. Preserve status fields owned by
	// provisioning, agent-health, and future feature controllers.
	merged := make(map[string]any, len(status)+1)
	for key, value := range status {
		merged[key] = value
	}
	merged["inventory"] = inventory
	if _, hasPhase := merged["phase"]; !hasPhase {
		merged["phase"] = "Available"
	}
	return merged
}

func machineNameForLocation(location apigen.MachineLocation) string {
	location = canonicalLocation(location)
	digest := sha256.Sum256([]byte(location.SwitchMac + "\x00" + location.LldpPort))
	return "machine-" + hex.EncodeToString(digest[:6])
}

func canonicalLocation(location apigen.MachineLocation) apigen.MachineLocation {
	return apigen.MachineLocation{
		LldpPort:  strings.TrimSpace(location.LldpPort),
		SwitchMac: strings.ToLower(strings.TrimSpace(location.SwitchMac)),
	}
}

func machineLocation(groups []apigen.MachineReportLLDPInterfaceGroup) apigen.MachineLocation {
	var location apigen.MachineLocation
	for _, group := range groups {
		for _, iface := range group.Interface {
			if location.LldpPort == "" {
				for _, port := range iface.Ports {
					for _, id := range port.IDs {
						if id.Value != "" {
							location.LldpPort = id.Value
							break
						}
					}
				}
			}
			if location.SwitchMac == "" {
				for _, chassis := range iface.Chassis {
					for _, id := range chassis.IDs {
						if strings.EqualFold(id.Type, "mac") && id.Value != "" {
							location.SwitchMac = id.Value
							break
						}
					}
				}
			}
			if location.LldpPort != "" && location.SwitchMac != "" {
				return location
			}
		}
	}
	return location
}
