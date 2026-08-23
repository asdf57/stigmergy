package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type MachineReportReconciler struct {
	store store.Store
}

func NewMachineReportReconciler(store store.Store) *MachineReportReconciler {
	return &MachineReportReconciler{
		store: store,
	}
}

func (r *MachineReportReconciler) Reconcile(ctx context.Context, event controller.Request) error {
	rsrc, err := r.store.Get(ctx, registry.MachineReportResource.Kind, event.Name)
	if errors.Is(err, store.ErrNotFound) {
		// this is an example of how you'd handle a delete case...
		// for now we do nothing
		slog.Info("MachineReport not found, skipping", "name", event.Name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get MachineReport %q: %w", event.Name, err)
	}
	slog.Info("reconciling MachineReport", "name", event.Name)

	report, err := registry.MachineReportResource.Decode(rsrc)
	if err != nil {
		return fmt.Errorf("decode MachineReport %q: %w", event.Name, err)
	}
	reportRevision, err := strconv.ParseInt(report.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse MachineReport %q resource version: %w", event.Name, err)
	}

	slog.Debug("decoded MachineReport", "name", report.Metadata.Name, "observedAt", report.Spec.ObservedAt, "interfaces", len(report.Spec.Interfaces))

	location := machineLocation(report.Spec.LLDPInfo)
	slog.Debug("searching for Machine", "reportName", report.Metadata.Name, "lldpPort", location.LldpPort, "switchMac", location.SwitchMac)

	desired := registry.Machine{
		Metadata: resource.Metadata{
			Name: report.Metadata.Name,
		},
		Spec: apigen.MachineSpec{
			Location:   location,
			ObservedAt: report.Spec.ObservedAt,
			Storage:    report.Spec.Storage,
			System:     report.Spec.System,
			Cpu:        report.Spec.Cpu,
			Interfaces: report.Spec.Interfaces,
			LLDPInfo:   report.Spec.LLDPInfo,
		},
	}

	// Match the stable report name first and LLDP location as a fallback.
	machines, err := r.store.List(ctx, registry.MachineResource.Kind)
	if err != nil {
		return fmt.Errorf("list Machines: %w", err)
	}

	var existing *resource.Resource
	for index := range machines.Items {
		m := &machines.Items[index]
		machine, err := registry.MachineResource.Decode(*m)
		if err != nil {
			return fmt.Errorf("decode Machine %q: %w", m.Metadata.Name, err)
		}

		slog.Debug("checking Machine", "name", machine.Metadata.Name, "lldpPort", machine.Spec.Location.LldpPort, "lldpSwitchMac", machine.Spec.Location.SwitchMac)

		sameName := machine.Metadata.Name == report.Metadata.Name
		sameLocation := location.LldpPort != "" && location.SwitchMac != "" && machine.Spec.Location == location
		if sameName || sameLocation {
			slog.Debug("found existing Machine", "name", machine.Metadata.Name)
			existing = m
			break
		}
	}

	if existing != nil {
		desired.Metadata = existing.Metadata
		desired.Metadata.Annotations = make(map[string]string, len(existing.Metadata.Annotations))
		for key, value := range existing.Metadata.Annotations {
			if key == "homelab.io/source-report-uid" {
				continue
			}
			desired.Metadata.Annotations[key] = value
		}
		if len(desired.Metadata.Annotations) == 0 {
			desired.Metadata.Annotations = nil
		}
	}

	candidate, err := desired.Encode()
	if err != nil {
		return fmt.Errorf("encode Machine %q: %w", report.Metadata.Name, err)
	}
	if existing != nil {
		machine, err := registry.MachineResource.Decode(*existing)
		if err != nil {
			return fmt.Errorf("decode Machine %q: %w", existing.Metadata.Name, err)
		}
		if machine.Spec.ObservedAt.After(report.Spec.ObservedAt) {
			slog.Info("discarding stale MachineReport", "name", report.Metadata.Name, "observedAt", report.Spec.ObservedAt, "machineObservedAt", machine.Spec.ObservedAt)
			return r.consumeReport(ctx, report, reportRevision)
		}
		if reflect.DeepEqual(existing.Spec, candidate.Spec) && reflect.DeepEqual(existing.Metadata.Annotations, candidate.Metadata.Annotations) {
			return r.consumeReport(ctx, report, reportRevision)
		}
		if err := resource.Validate(candidate); err != nil {
			return fmt.Errorf("validate Machine %q update: %w", candidate.Metadata.Name, err)
		}
		revision, err := strconv.ParseInt(existing.Metadata.ResourceVersion, 10, 64)
		if err != nil {
			return fmt.Errorf("parse Machine %q resource version: %w", candidate.Metadata.Name, err)
		}
		updated, err := r.store.Update(ctx, candidate, revision)
		if err != nil {
			return fmt.Errorf("update Machine %q: %w", candidate.Metadata.Name, err)
		}
		slog.Info("updated Machine", "name", updated.Metadata.Name, "uid", updated.Metadata.UID)
		return r.consumeReport(ctx, report, reportRevision)
	}

	if err := resource.ValidateCreate(candidate); err != nil {
		return fmt.Errorf("validate Machine %q: %w", report.Metadata.Name, err)
	}

	created, err := r.store.Create(ctx, candidate)
	if err != nil {
		return fmt.Errorf("create Machine %q: %w", report.Metadata.Name, err)
	}
	slog.Info("created Machine", "name", created.Metadata.Name, "uid", created.Metadata.UID)

	return r.consumeReport(ctx, report, reportRevision)
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
