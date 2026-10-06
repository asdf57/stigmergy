package api

import (
	"encoding/json"
	"testing"

	"github.com/asdf57/stigmergy/internal/resource"
)

func cloneMap(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	var copy map[string]any
	_ = json.Unmarshal(data, &copy)
	return copy
}

func provisionHost() resource.Resource {
	return resource.Resource{Kind: "Server", Metadata: resource.Metadata{UID: "server-uid"}, Spec: map[string]any{
		"provisioning":    map[string]any{"enabled": true, "reprovision": 0, "targetDisk": "/dev/disk/by-id/disk"},
		"operatingSystem": map[string]any{"distribution": "arch"},
	}, Status: map[string]any{"machineRef": map[string]any{"name": "machine", "uid": "machine-uid"},
		"bootISORef": map[string]any{"name": "iso", "uid": "iso-uid"},
		"hostSSH":    map[string]any{"keyPairRef": map[string]any{"name": "key", "uid": "key-uid"}}}}
}

func TestReprovisionCounterAndPinnedInputs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		count       any
		conditional bool
		phase       string
		want        bool
	}{
		{"unchanged", 0, false, "", true}, {"increment", 1, true, "", true}, {"unconditional", 1, false, "", false},
		{"negative", -1, true, "", false}, {"jump", 2, true, "", false}, {"fraction", 0.5, true, "", false},
		{"overflow", json.Number("9223372036854775808"), true, "", false}, {"active", 1, true, "Installing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := provisionHost()
			if tc.phase != "" {
				host.Status["provisioning"] = map[string]any{"phase": tc.phase}
			}
			next := cloneMap(host.Spec)
			object(next["provisioning"])["reprovision"] = tc.count
			if err := validateProvisioningSpec(host, next, tc.conditional); (err == nil) != tc.want {
				t.Fatalf("error=%v", err)
			}
		})
	}
	host := provisionHost()
	host.Status["provisioning"] = map[string]any{"phase": "AwaitingLive"}
	next := cloneMap(host.Spec)
	object(next["operatingSystem"])["distribution"] = "debian"
	if validateProvisioningSpec(host, next, true) == nil {
		t.Fatal("changed active OS")
	}
	next = cloneMap(host.Spec)
	next["reconciliation"] = map[string]any{"paused": true}
	object(next["provisioning"])["enabled"] = false
	if err := validateProvisioningSpec(host, next, true); err != nil {
		t.Fatal(err)
	}
}

func TestProvisioningAttemptTransitionsAndNoFalseSuccess(t *testing.T) {
	host := provisionHost()
	claim := cloneMap(host.Status)
	snapshot := map[string]any{"serverUID": "server-uid", "machineRef": host.Status["machineRef"], "keyPairRef": object(host.Status["hostSSH"])["keyPairRef"], "isoRef": host.Status["bootISORef"], "targetDisk": "/dev/disk/by-id/disk"}
	claim["provisioning"] = map[string]any{"attemptID": "attempt", "phase": "PreparingBoot", "provisioned": false, "maintenance": true, "snapshot": snapshot, "requestedReprovision": 0, "observedReprovision": 0}
	if err := validateProvisioningStatus(host, claim); err != nil {
		t.Fatal(err)
	}
	host.Status = claim
	bad := cloneMap(claim)
	object(bad["provisioning"])["phase"] = "Succeeded"
	object(bad["provisioning"])["provisioned"] = true
	if validateProvisioningStatus(host, bad) == nil {
		t.Fatal("skipped installed verification")
	}
	bad = cloneMap(claim)
	object(object(bad["provisioning"])["snapshot"])["serverUID"] = "replacement"
	if validateProvisioningStatus(host, bad) == nil {
		t.Fatal("changed immutable snapshot")
	}
	for _, phase := range []string{"AwaitingLive", "Installing", "AwaitingInstalled", "Verifying", "Succeeded"} {
		next := cloneMap(host.Status)
		p := object(next["provisioning"])
		p["phase"] = phase
		if phase == "Succeeded" {
			p["maintenance"] = false
			p["netbootArmed"] = false
			p["provisioned"] = true
		}
		if err := validateProvisioningStatus(host, next); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		host.Status = next
	}
	bad = cloneMap(host.Status)
	object(bad["provisioning"])["attemptID"] = "again"
	object(bad["provisioning"])["phase"] = "PreparingBoot"
	if validateProvisioningStatus(host, bad) == nil {
		t.Fatal("replayed the same successful request")
	}
}
