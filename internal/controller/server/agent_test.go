package server

import (
	"testing"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
)

func TestAgentReportProjectionUsesReceiptTimeAndClearsOldBinding(t *testing.T) {
	received := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	agentClock := received.Add(24 * time.Hour)
	machine := &registry.Machine{Status: &apigen.MachineStatus{LastSeenTime: &received, Inventory: &apigen.MachineReportSpec{ObservedAt: agentClock}}}
	status := &apigen.ServerStatus{}
	applyAgentReportStatus(status, machine)
	if status.Agent == nil || !status.Agent.LastSeenTime.Equal(received) {
		t.Fatal("agent report projection did not use API receipt time", status.Agent)
	}
	applyAgentReportStatus(status, nil)
	if status.Agent != nil {
		t.Fatal("unbound Server retained another machine's heartbeat")
	}
	applyAgentReportStatus(status, &registry.Machine{})
	if status.Agent != nil {
		t.Fatal("missing heartbeat was invented")
	}
}
