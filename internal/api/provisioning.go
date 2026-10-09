package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"strconv"
	"strings"
)

func object(value any) map[string]any { result, _ := value.(map[string]any); return result }

func activeProvisioning(status map[string]any) bool {
	p := object(status["provisioning"])
	return p["maintenance"] == true || p["activeRunRef"] != nil
}

// Deletion releases only this run's references, never a newer reservation.
// It changes API ownership, not the node's boot selection or disk state.
func cloneStatusForRunDeletion(status map[string]any, run resource.Resource) map[string]any {
	patch := map[string]any{}
	p := object(status["provisioning"])
	for _, field := range []string{"activeRunRef", "lastRunRef", "lastSuccessfulRunRef"} {
		ref := object(p[field])
		if ref["name"] == run.Metadata.Name && ref["uid"] == run.Metadata.UID {
			patch[field] = nil
			if field == "activeRunRef" {
				patch["maintenance"] = false
			}
		}
	}
	return applyJSONMergePatch(status, map[string]any{"provisioning": patch})
}

func validateProvisioningSpec(existing resource.Resource, spec map[string]any, _ bool) error {
	if existing.Kind != "Server" || !activeProvisioning(existing.Status) {
		return nil
	}
	for _, field := range []string{"machineSelector", "boot", "operatingSystem", "sshCertificateAuthorityRef", "users", "groups", "packages", "sysctls", "featureFlags", "networking", "hostName", "domainName"} {
		if !resource.EqualJSON(existing.Spec[field], spec[field]) {
			return fmt.Errorf("%s is pinned by the active ProvisioningRun", field)
		}
	}
	return nil
}

func diskDeviceID(disk map[string]any) string {
	if value, _ := disk["wwn"].(string); value != "" {
		return "wwn:" + strings.ToLower(value)
	}
	if value, _ := disk["serial"].(string); value != "" {
		return "serial:" + value
	}
	return ""
}

func selectedDisk(machine resource.Resource, spec map[string]any) (map[string]any, error) {
	disks, _ := object(spec["storage"])["disks"].([]any)
	if len(disks) != 1 || object(disks[0])["role"] != "system" {
		return nil, fmt.Errorf("select exactly one system disk")
	}
	id, _ := object(disks[0])["deviceID"].(string)
	inventory, _ := object(machine.Status["inventory"])["storage"].([]any)
	matches := []map[string]any{}
	for _, raw := range inventory {
		disk := object(raw)
		if id != "" && diskDeviceID(disk) == id {
			matches = append(matches, disk)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("disk identity is missing or ambiguous in the bound Machine inventory")
	}
	disk := matches[0]
	if disk["type"] != "disk" || (disk["tran"] != "sata" && disk["tran"] != "ata" && disk["tran"] != "nvme") {
		return nil, fmt.Errorf("USB and nonphysical disks are not supported")
	}
	encoded, _ := json.Marshal(disk["size"])
	size, err := strconv.ParseInt(string(encoded), 10, 64)
	if err != nil || size <= 0 {
		return nil, fmt.Errorf("disk capacity must be a positive int64")
	}
	result := map[string]any{}
	for _, field := range []string{"serial", "wwn", "size", "model", "tran"} {
		result[field] = disk[field]
	}
	return result, nil
}

func (s *Server) validateRunReservation(ctx context.Context, run resource.Resource, next map[string]any) error {
	if resource.EqualJSON(run.Status, next) {
		return nil
	}
	ref := object(run.Spec["serverRef"])
	owner, err := s.store.Get(ctx, "Server", fmt.Sprint(ref["name"]))
	if err != nil {
		return err
	}
	active := object(object(owner.Status["provisioning"])["activeRunRef"])
	if owner.Metadata.UID != ref["uid"] || owner.Metadata.DeletionTimestamp != nil || active["uid"] != run.Metadata.UID || active["name"] != run.Metadata.Name || !resource.EqualJSON(owner.Status["machineRef"], run.Spec["machineRef"]) {
		return fmt.Errorf("run no longer owns the bound Server")
	}
	if run.Status["phase"] == "Pending" && next["phase"] == "PreparingBoot" {
		snapshot := object(next["snapshot"])
		if object(owner.Spec["provisioning"])["enabled"] != true || object(owner.Spec["reconciliation"])["paused"] == true || !resource.EqualJSON(snapshot["keyPairRef"], object(owner.Status["hostSSH"])["keyPairRef"]) || !resource.EqualJSON(snapshot["isoRef"], owner.Status["bootISORef"]) {
			return fmt.Errorf("claim does not match enabled Server dependencies")
		}
	}
	return nil
}

func (s *Server) createProvisioningRun(ctx context.Context, candidate resource.Resource) (resource.Resource, error) {
	ref := object(candidate.Spec["serverRef"])
	server, err := s.store.Get(ctx, "Server", fmt.Sprint(ref["name"]))
	if err != nil {
		return resource.Resource{}, err
	}
	if server.Metadata.UID != ref["uid"] || server.Metadata.DeletionTimestamp != nil || activeProvisioning(server.Status) {
		return resource.Resource{}, fmt.Errorf("%w: Server identity changed or another run owns it", store.ErrConflict)
	}
	if !resource.EqualJSON(candidate.Spec["serverGeneration"], server.Metadata.Generation) {
		return resource.Resource{}, fmt.Errorf("%w: Server desired configuration changed; review it again", store.ErrConflict)
	}
	if object(server.Spec["provisioning"])["enabled"] != true || object(server.Spec["reconciliation"])["paused"] == true {
		return resource.Resource{}, fmt.Errorf("%w: provisioning disabled or reconciliation paused", store.ErrConflict)
	}
	hostSSH := object(server.Status["hostSSH"])
	if hostSSH["phase"] != "Ready" || hostSSH["keyReady"] != true || hostSSH["keyPairRef"] == nil || !resource.EqualJSON(hostSSH["keyPairRef"], hostSSH["installedKeyPairRef"]) {
		return resource.Resource{}, fmt.Errorf("%w: establish managed SSH identity before reserving provisioning", store.ErrConflict)
	}
	if !resource.EqualJSON(server.Status["machineRef"], candidate.Spec["machineRef"]) {
		return resource.Resource{}, fmt.Errorf("%w: Machine binding changed", store.ErrConflict)
	}
	machineRef := object(candidate.Spec["machineRef"])
	machine, err := s.store.Get(ctx, "Machine", fmt.Sprint(machineRef["name"]))
	if err != nil {
		return resource.Resource{}, err
	}
	if machine.Metadata.UID != machineRef["uid"] || machine.Metadata.DeletionTimestamp != nil || !resource.EqualJSON(machine.Status["serverRef"], candidate.Spec["serverRef"]) {
		return resource.Resource{}, fmt.Errorf("%w: stale Machine identity or reciprocal binding", store.ErrConflict)
	}
	disk, err := selectedDisk(machine, candidate.Spec)
	if err != nil {
		return resource.Resource{}, fmt.Errorf("%w: %s", store.ErrConflict, err)
	}
	candidate.Status = map[string]any{"phase": "Pending", "maintenance": true, "selectedDisk": disk, "message": "Waiting for the provisioning operator"}
	atomic, ok := s.store.(store.AtomicCreator)
	if !ok {
		return resource.Resource{}, fmt.Errorf("store does not support atomic resource reservations")
	}
	return atomic.CreateWithStatus(ctx, candidate, server, func(created resource.Resource) map[string]any {
		return applyJSONMergePatch(server.Status, map[string]any{"provisioning": map[string]any{
			"provisioned": object(server.Status["provisioning"])["provisioned"] == true,
			"maintenance": true, "activeRunRef": map[string]any{"name": created.Metadata.Name, "uid": created.Metadata.UID},
			"lastRunRef": map[string]any{"name": created.Metadata.Name, "uid": created.Metadata.UID},
		}})
	})
}

func validateProvisioningRunStatus(current resource.Resource, next map[string]any) error {
	old := current.Status
	from, to := fmt.Sprint(old["phase"]), fmt.Sprint(next["phase"])
	if resource.EqualJSON(old, next) {
		return nil
	}
	if !resource.EqualJSON(old["selectedDisk"], next["selectedDisk"]) {
		return fmt.Errorf("selected disk observation is immutable")
	}
	allowed := map[string]string{"Pending": "PreparingBoot", "PreparingBoot": "AwaitingLive", "AwaitingLive": "Installing", "Installing": "AwaitingInstalled", "AwaitingInstalled": "Verifying", "Verifying": "Succeeded"}
	repaired := from == "Blocked" && to == "AwaitingInstalled" && old["maintenance"] == true && next["maintenance"] == true && next["netbootArmed"] == false && next["bootTarget"] == "installed" && old["liveBootID"] != nil && old["liveBootID"] != "" && resource.EqualJSON(old["liveBootID"], next["liveBootID"])
	if from != to && allowed[from] != to && !(to == "Blocked" && from != "Succeeded" && from != "Blocked") && !repaired {
		return fmt.Errorf("invalid ProvisioningRun checkpoint transition")
	}
	if from == "Pending" && to == "PreparingBoot" {
		snapshot := object(next["snapshot"])
		if next["attemptID"] != current.Metadata.UID || snapshot["serverUID"] != object(current.Spec["serverRef"])["uid"] || !resource.EqualJSON(snapshot["machineRef"], current.Spec["machineRef"]) || !resource.EqualJSON(snapshot["diskIdentity"], old["selectedDisk"]) {
			return fmt.Errorf("claim must match run, Server, Machine and selected disk identities")
		}
	} else if old["attemptID"] != nil {
		for _, field := range []string{"attemptID", "snapshot", "observedServerGeneration", "backendRunID", "startedAt"} {
			if !resource.EqualJSON(old[field], next[field]) {
				return fmt.Errorf("claimed %s is immutable", field)
			}
		}
	}
	if to != "Pending" && to != "Blocked" && (next["attemptID"] != current.Metadata.UID || next["snapshot"] == nil) {
		return fmt.Errorf("checkpoint requires the claimed run snapshot")
	}
	if to != "Succeeded" && to != "Blocked" && next["maintenance"] != true {
		return fmt.Errorf("active run must retain maintenance")
	}
	if to == "Succeeded" && (next["maintenance"] == true || next["netbootArmed"] == true || next["completedAt"] == nil) {
		return fmt.Errorf("success requires installed verification and boot cleanup")
	}
	return nil
}

func (s *Server) validateProvisioningStatus(ctx context.Context, current resource.Resource, status map[string]any) error {
	old, next := object(current.Status["provisioning"]), object(status["provisioning"])
	if resource.EqualJSON(old, next) {
		return nil
	}
	if old["provisioned"] == true && next["provisioned"] != true {
		return fmt.Errorf("retain the verified installation observation")
	}
	if !resource.EqualJSON(old["lastRunRef"], next["lastRunRef"]) {
		return fmt.Errorf("lastRunRef is assigned only by ProvisioningRun creation")
	}
	if resource.EqualJSON(old["activeRunRef"], next["activeRunRef"]) && resource.EqualJSON(old["maintenance"], next["maintenance"]) && resource.EqualJSON(old["lastSuccessfulRunRef"], next["lastSuccessfulRunRef"]) && resource.EqualJSON(old["provisioned"], next["provisioned"]) {
		return nil
	}
	ref := object(old["activeRunRef"])
	if ref == nil || next["activeRunRef"] != nil || next["maintenance"] == true {
		return fmt.Errorf("reservations are assigned only by ProvisioningRun creation")
	}
	run, err := s.store.Get(ctx, "ProvisioningRun", fmt.Sprint(ref["name"]))
	if err != nil {
		return err
	}
	if run.Metadata.UID != ref["uid"] || object(run.Spec["serverRef"])["uid"] != current.Metadata.UID || run.Status["maintenance"] == true || run.Status["netbootArmed"] == true || (run.Status["phase"] != "Succeeded" && run.Status["phase"] != "Blocked") {
		return fmt.Errorf("reserved run has not completed boot cleanup")
	}
	if run.Status["phase"] == "Succeeded" {
		if next["provisioned"] != true || !resource.EqualJSON(next["lastSuccessfulRunRef"], ref) {
			return fmt.Errorf("record the successful reserved run")
		}
	} else if !resource.EqualJSON(next["lastSuccessfulRunRef"], old["lastSuccessfulRunRef"]) || !resource.EqualJSON(next["provisioned"], old["provisioned"]) {
		return fmt.Errorf("failed run cannot change the last verified installation")
	}
	return nil
}
