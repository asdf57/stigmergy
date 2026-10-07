package api

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/asdf57/stigmergy/internal/resource"
)

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func counter(value any) (int64, error) {
	if value == nil {
		return 0, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	result, err := strconv.ParseInt(string(encoded), 10, 64)
	if err != nil || result < 0 {
		return 0, fmt.Errorf("counter must be a nonnegative int64")
	}
	return result, nil
}

func activeProvisioning(status map[string]any) bool {
	phase, _ := object(status["provisioning"])["phase"].(string)
	switch phase {
	case "PreparingBoot", "AwaitingLive", "Installing", "AwaitingInstalled", "Verifying":
		return true
	}
	return false
}

// Ordinary edits never authorize another wipe. Freeze destructive inputs while
// an attempt owns them, but allow pause/disable and unrelated metadata edits.
func validateProvisioningSpec(existing resource.Resource, spec map[string]any, conditional bool) error {
	if existing.Kind != "Server" {
		return nil
	}
	previous, err := counter(object(existing.Spec["provisioning"])["reprovision"])
	if err != nil {
		return err
	}
	next, err := counter(object(spec["provisioning"])["reprovision"])
	if err != nil {
		return err
	}
	if next != previous {
		if !conditional {
			return fmt.Errorf("reprovision changes require If-Match")
		}
		if previous == math.MaxInt64 || next != previous+1 {
			return fmt.Errorf("reprovision must increase by exactly one")
		}
		if activeProvisioning(existing.Status) || object(existing.Status["provisioning"])["maintenance"] == true {
			return fmt.Errorf("finish the active attempt and boot-selection cleanup before another request")
		}
	}
	if activeProvisioning(existing.Status) || object(existing.Status["provisioning"])["maintenance"] == true {
		for _, field := range []string{"machineSelector", "boot", "operatingSystem", "sshCertificateAuthorityRef", "users", "groups", "packages", "sysctls", "featureFlags", "networking", "hostName", "domainName"} {
			if !resource.EqualJSON(existing.Spec[field], spec[field]) {
				return fmt.Errorf("%s is pinned by the active provisioning attempt", field)
			}
		}
		if !resource.EqualJSON(object(existing.Spec["provisioning"])["targetDisk"], object(spec["provisioning"])["targetDisk"]) {
			return fmt.Errorf("targetDisk is pinned by the active provisioning attempt")
		}
	}
	return nil
}

func validateProvisioningStatus(current resource.Resource, status map[string]any) error {
	old, next := object(current.Status["provisioning"]), object(status["provisioning"])
	if resource.EqualJSON(old, next) {
		return nil
	}
	requested, err := counter(object(current.Spec["provisioning"])["reprovision"])
	if err != nil {
		return err
	}
	observed, err := counter(next["observedReprovision"])
	if err != nil {
		return err
	}
	oldObserved, err := counter(old["observedReprovision"])
	if err != nil {
		return err
	}
	attemptCounter, err := counter(next["requestedReprovision"])
	if err != nil {
		return err
	}
	if observed < oldObserved || observed > requested || attemptCounter > requested {
		return fmt.Errorf("invalid provisioning counter observation")
	}
	if old["provisioned"] == true && next["provisioned"] != true {
		return fmt.Errorf("retain the last verified installation observation")
	}
	newAttempt := next["attemptID"] != old["attemptID"]
	if activeProvisioning(status) && (next["attemptID"] == nil || next["attemptID"] == "" || next["snapshot"] == nil || next["maintenance"] != true) {
		return fmt.Errorf("active provisioning requires an owned attempt, snapshot and maintenance reservation")
	}
	if newAttempt {
		if activeProvisioning(current.Status) || old["maintenance"] == true {
			return fmt.Errorf("another attempt still owns this Server")
		}
		if next["phase"] != "PreparingBoot" || next["attemptID"] == nil || next["attemptID"] == "" || attemptCounter != requested {
			return fmt.Errorf("new attempts must claim the current request in PreparingBoot")
		}
		if object(current.Spec["provisioning"])["enabled"] != true || object(current.Spec["reconciliation"])["paused"] == true {
			return fmt.Errorf("provisioning is disabled or paused")
		}
		oldCounter, _ := counter(old["requestedReprovision"])
		if old["attemptID"] != nil && attemptCounter <= oldCounter {
			return fmt.Errorf("a terminal attempt requires a new reprovision increment")
		}
		if old["provisioned"] == true && attemptCounter <= oldObserved {
			return fmt.Errorf("installation already observed; increment reprovision")
		}
		if object(next["snapshot"])["serverUID"] != current.Metadata.UID {
			return fmt.Errorf("attempt snapshot must match the Server UID")
		}
		snapshot := object(next["snapshot"])
		if !resource.EqualJSON(snapshot["machineRef"], current.Status["machineRef"]) ||
			!resource.EqualJSON(snapshot["keyPairRef"], object(current.Status["hostSSH"])["keyPairRef"]) ||
			!resource.EqualJSON(snapshot["isoRef"], current.Status["bootISORef"]) ||
			!resource.EqualJSON(snapshot["targetDisk"], object(current.Spec["provisioning"])["targetDisk"]) {
			return fmt.Errorf("attempt snapshot does not match bound dependencies or target disk")
		}
	}
	if !newAttempt && old["attemptID"] != nil {
		for _, field := range []string{"snapshot", "requestedReprovision", "backendRunID", "observedServerGeneration"} {
			if !resource.EqualJSON(old[field], next[field]) {
				return fmt.Errorf("attempt %s is immutable", field)
			}
		}
		from, to := old["phase"], next["phase"]
		allowed := map[string]string{"PreparingBoot": "AwaitingLive", "AwaitingLive": "Installing", "Installing": "AwaitingInstalled", "AwaitingInstalled": "Verifying", "Verifying": "Succeeded"}
		// Explicit repair may complete a partial installation, but cannot return
		// to Installing/replay erasure. The operator must attest the completed
		// staged marker before making this generic status update; normal installed
		// boot and marker verification still gate success.
		repaired := from == "Blocked" && to == "AwaitingInstalled" && old["maintenance"] == true &&
			next["maintenance"] == true && next["netbootArmed"] == false && next["bootTarget"] == "installed" &&
			old["liveBootID"] != nil && old["liveBootID"] != "" && resource.EqualJSON(old["liveBootID"], next["liveBootID"])
		if from != to && to != "Blocked" && allowed[fmt.Sprint(from)] != to && !repaired {
			return fmt.Errorf("invalid provisioning stage transition")
		}
	}
	if observed != oldObserved || (next["provisioned"] == true && old["provisioned"] != true) || next["phase"] == "Succeeded" {
		if next["attemptID"] == nil || (old["phase"] != "Verifying" && old["phase"] != "Succeeded") || newAttempt {
			return fmt.Errorf("success requires completing the same Verifying attempt")
		}
		if next["phase"] != "Succeeded" || next["provisioned"] != true || observed != attemptCounter || next["maintenance"] == true || next["netbootArmed"] == true {
			return fmt.Errorf("only a completed installed verification may advance observedReprovision")
		}
	}
	return nil
}
