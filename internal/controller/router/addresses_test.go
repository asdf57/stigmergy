package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"testing"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
)

func testAddress(name, address, network string) apigen.RouterAddress {
	return apigen.RouterAddress{Name: name, Address: address, Network: network, Interface: "bridge"}
}

func TestPlanPreservesExistingAddressesAndPointToPointManagement(t *testing.T) {
	desired := []apigen.RouterAddress{
		testAddress("bridge-1", "10.1.1.1/8", "10.0.0.0"),
		testAddress("services", "10.0.2.1/32", "10.0.2.0"),
		testAddress("metallb", "10.0.3.1/24", "10.0.3.0"),
	}
	existing := []RouterOSAddress{
		{ID: "*1", Address: "10.1.1.1/8", Network: "10.0.0.0", Interface: "bridge", Comment: "existing LAN"},
		{ID: "*2", Address: "10.0.2.1/32", Network: "10.0.2.0", Interface: "bridge"},
		{ID: "*3", Address: "10.0.0.178/24", Network: "10.0.0.0", Interface: "ISP", Dynamic: true},
		{ID: "*4", Address: "192.168.1.111/24", Network: "192.168.1.0", Interface: "ISP"},
	}
	snapshot := slices.Clone(existing)
	plan, err := planAddresses("router-uid", desired, existing, netip.MustParseAddr("10.0.2.1"))
	if err != nil || len(plan) != 1 || plan[0].action != "create" || plan[0].after.Address != "10.0.3.1/24" {
		t.Fatalf("plan = %#v, err = %v; want only the missing MetalLB address", plan, err)
	}
	if !reflect.DeepEqual(snapshot, existing) {
		t.Fatal("planning modified the observed slice")
	}
}

func TestPlanUpdatesByOwnedNameAndRemovesOnlyOwnedStaticEntries(t *testing.T) {
	existing := []RouterOSAddress{
		{ID: "*1", Address: "10.0.3.1/24", Network: "10.0.3.0", Interface: "bridge", Comment: addressOwnerPrefix("uid") + "services"},
		{ID: "*2", Address: "10.0.4.1/24", Network: "10.0.4.0", Interface: "bridge", Comment: addressOwnerPrefix("uid") + "obsolete"},
		{ID: "*3", Address: "10.0.5.1/24", Interface: "ISP", Dynamic: true, Comment: addressOwnerPrefix("uid") + "dynamic"},
		{ID: "*4", Address: "10.0.6.1/24", Comment: addressOwnerPrefix("another-router") + "foreign"},
	}
	desired := []apigen.RouterAddress{
		testAddress("services", "10.0.7.1/24", "10.0.7.0"),
		testAddress("new", "10.0.8.1/24", "10.0.8.0"),
	}
	plan, err := planAddresses("uid", desired, existing, netip.MustParseAddr("10.0.2.1"))
	if err != nil || len(plan) != 3 {
		t.Fatalf("plan = %#v, err = %v", plan, err)
	}
	if plan[0].action != "create" || plan[1].action != "update" || plan[1].after.ID != "*1" || plan[2].action != "remove" || plan[2].before.ID != "*2" {
		t.Fatalf("unsafe order or unexpected ownership: %#v", plan)
	}
}

func TestPlanRejectsUnsafeChangesBeforeAnyWrites(t *testing.T) {
	management := netip.MustParseAddr("10.0.2.1")
	address := testAddress("metallb", "10.0.3.1/24", "10.0.3.0")
	tests := []struct {
		name     string
		desired  []apigen.RouterAddress
		existing []RouterOSAddress
	}{
		{"unmanaged conflict", []apigen.RouterAddress{address}, []RouterOSAddress{{ID: "*1", Address: "10.0.3.1/25", Interface: "bridge"}}},
		{"dynamic conflict", []apigen.RouterAddress{address}, []RouterOSAddress{{ID: "*1", Address: "10.0.3.1/24", Network: "10.0.3.0", Interface: "bridge", Dynamic: true}}},
		{"foreign owner", []apigen.RouterAddress{address}, []RouterOSAddress{{ID: "*1", Address: "10.0.3.1/24", Network: "10.0.3.0", Interface: "bridge", Comment: addressOwnerPrefix("other") + "metallb"}}},
		{"duplicate name", []apigen.RouterAddress{address, address}, nil},
		{"duplicate IP", []apigen.RouterAddress{address, testAddress("another", "10.0.3.1/25", "10.0.3.0")}, nil},
		{"invalid network", []apigen.RouterAddress{testAddress("bad", "10.0.3.1/24", "10.0.0.0")}, nil},
		{"invalid CIDR", []apigen.RouterAddress{testAddress("bad", "not-an-address", "10.0.3.0")}, nil},
		{"network host address", []apigen.RouterAddress{testAddress("bad", "10.0.3.0/24", "10.0.3.0")}, nil},
		{"broadcast host address", []apigen.RouterAddress{testAddress("bad", "10.0.3.255/24", "10.0.3.0")}, nil},
		{"invalid name", []apigen.RouterAddress{testAddress("bad:name", "10.0.3.1/24", "10.0.3.0")}, nil},
		{"remove management", nil, []RouterOSAddress{{ID: "*1", Address: "10.0.2.1/32", Comment: addressOwnerPrefix("uid") + "management"}}},
		{"remove broad management route", nil, []RouterOSAddress{{ID: "*1", Address: "10.1.1.1/8", Comment: addressOwnerPrefix("uid") + "lan"}}},
		{"update management", []apigen.RouterAddress{testAddress("management", "10.0.2.1/24", "10.0.2.0")}, []RouterOSAddress{{ID: "*1", Address: "10.0.2.1/32", Network: "10.0.2.0", Interface: "bridge", Comment: addressOwnerPrefix("uid") + "management"}}},
		{"create management prefix", []apigen.RouterAddress{testAddress("management", "10.0.2.10/24", "10.0.2.0")}, nil},
		{"duplicate owners", []apigen.RouterAddress{address}, []RouterOSAddress{
			{ID: "*1", Comment: addressOwnerPrefix("uid") + "metallb"}, {ID: "*2", Comment: addressOwnerPrefix("uid") + "metallb"},
		}},
		{"missing owned ID", nil, []RouterOSAddress{{Address: "10.0.3.1/24", Comment: addressOwnerPrefix("uid") + "obsolete"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := planAddresses("uid", test.desired, test.existing, management)
			if err == nil || plan != nil {
				t.Fatalf("plan = %#v, error = %v; want refusal with no partial plan", plan, err)
			}
		})
	}
}

// addressClient simulates actual RouterOS changes and can expose concurrent edits.
type addressClient struct {
	addresses   []RouterOSAddress
	writes      []string
	lists       int
	beforeList  func(int)
	writeError  error
	ignoreWrite bool
}

func (*addressClient) Probe(context.Context) error { return nil }
func (c *addressClient) ListAddresses(context.Context) ([]RouterOSAddress, error) {
	c.lists++
	if c.beforeList != nil {
		c.beforeList(c.lists)
	}
	return slices.Clone(c.addresses), nil
}
func (c *addressClient) AddSubnet(_ context.Context, address RouterOSAddress) error {
	c.writes = append(c.writes, "create")
	if c.writeError == nil && !c.ignoreWrite {
		address.ID = fmt.Sprintf("*new%d", len(c.writes))
		c.addresses = append(c.addresses, address)
	}
	return c.writeError
}
func (c *addressClient) UpdateSubnet(_ context.Context, address RouterOSAddress) error {
	c.writes = append(c.writes, "update")
	if c.writeError == nil && !c.ignoreWrite {
		for i, existing := range c.addresses {
			if existing.ID == address.ID {
				c.addresses[i] = address
			}
		}
	}
	return c.writeError
}
func (c *addressClient) RemoveSubnet(_ context.Context, id string) error {
	c.writes = append(c.writes, "remove")
	if c.writeError == nil && !c.ignoreWrite {
		c.addresses = slices.DeleteFunc(c.addresses, func(entry RouterOSAddress) bool { return entry.ID == id })
	}
	return c.writeError
}

func addressTestSetup(t *testing.T, desired []apigen.RouterAddress, client *addressClient) (*Reconciler, *fakeStore, controller.Request) {
	t.Helper()
	raw := testRouter("mikrotik", "credentials")
	router, err := registry.RouterResource.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	router.Spec.MgmtAddr = "10.0.2.1"
	router.Spec.Addresses = &desired
	raw, err = router.Encode()
	if err != nil {
		t.Fatal(err)
	}
	storage := newFakeStore(raw, testCredential("credentials", apigen.UsernamePasswordCredentialStatusPhaseReady))
	reconciler := NewReconcilerWithClientFactory(storage, func(string, string, string) RouterClient { return client })
	return reconciler, storage, controller.Request{Kind: raw.Kind, Name: raw.Metadata.Name}
}

func TestReconcileAddressesConvergesAndIsIdempotent(t *testing.T) {
	client := &addressClient{addresses: []RouterOSAddress{
		{ID: "*existing", Address: "10.0.3.1/24", Network: "10.0.3.0", Interface: "bridge"},
		{ID: "*update", Address: "10.0.4.1/24", Network: "10.0.4.0", Interface: "bridge", Comment: addressOwnerPrefix("mikrotik-uid") + "update"},
		{ID: "*remove", Address: "10.0.5.1/24", Network: "10.0.5.0", Interface: "bridge", Comment: addressOwnerPrefix("mikrotik-uid") + "old"},
	}}
	desired := []apigen.RouterAddress{testAddress("existing", "10.0.3.1/24", "10.0.3.0"),
		testAddress("new", "10.0.6.1/24", "10.0.6.0"), testAddress("update", "10.0.7.1/24", "10.0.7.0")}
	reconciler, storage, request := addressTestSetup(t, desired, client)
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(client.writes, []string{"create", "update", "remove"}) {
		t.Fatalf("writes = %v", client.writes)
	}
	assertRouterStatus(t, storage, request.Name, "Ready", "AddressesReconciled")
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(client.writes) != 3 || storage.statusUpdates != 1 {
		t.Fatalf("idempotent pass wrote again: writes=%v statusUpdates=%d", client.writes, storage.statusUpdates)
	}
	if client.addresses[0].Comment != "" {
		t.Fatal("adopted an unowned address")
	}
}

func TestReconcileRefusesConcurrentDesiredAndRouterOSChanges(t *testing.T) {
	for _, change := range []string{"generation", "ownership"} {
		t.Run(change, func(t *testing.T) {
			client := &addressClient{}
			desired := []apigen.RouterAddress{testAddress("metallb", "10.0.3.1/24", "10.0.3.0")}
			reconciler, storage, request := addressTestSetup(t, desired, client)
			client.beforeList = func(call int) {
				if change == "generation" && call == 2 {
					value := storage.resources["Router/mikrotik"]
					value.Metadata.Generation++
					value.Metadata.ResourceVersion = "6"
					storage.resources["Router/mikrotik"] = value
				}
				if change == "ownership" && call == 2 {
					client.addresses = append(client.addresses, RouterOSAddress{ID: "*foreign", Address: "10.0.3.1/25"})
				}
			}
			if err := reconciler.Reconcile(context.Background(), request); err == nil || len(client.writes) != 0 {
				t.Fatalf("error=%v writes=%v; want refusal before mutation", err, client.writes)
			}
		})
	}
}

func TestReconcileDoesNotRemoveAfterFailedCreateOrClaimUnverifiedSuccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		client addressClient
		reason string
	}{
		{"write failure", addressClient{writeError: errors.New("simulated write failure")}, "AddressMutationFailed"},
		{"write ignored", addressClient{ignoreWrite: true}, "AddressVerificationFailed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &test.client
			if test.name == "write failure" {
				client.addresses = []RouterOSAddress{{ID: "*old", Address: "10.0.4.1/24", Comment: addressOwnerPrefix("mikrotik-uid") + "obsolete"}}
			}
			reconciler, storage, request := addressTestSetup(t, []apigen.RouterAddress{testAddress("new", "10.0.3.1/24", "10.0.3.0")}, client)
			if err := reconciler.Reconcile(context.Background(), request); err == nil {
				t.Fatal("unexpected success")
			}
			if !slices.Equal(client.writes, []string{"create"}) {
				t.Fatalf("unsafe subsequent writes: %v", client.writes)
			}
			assertRouterStatus(t, storage, request.Name, "Failed", test.reason)
		})
	}
}

func TestReconcileExplicitEmptyListPreservesUnownedAndDynamicAddresses(t *testing.T) {
	client := &addressClient{addresses: []RouterOSAddress{
		{ID: "*owned", Address: "10.0.3.1/24", Comment: addressOwnerPrefix("mikrotik-uid") + "old"},
		{ID: "*management", Address: "10.0.2.1/32", Network: "10.0.2.0", Interface: "bridge"},
		{ID: "*dynamic", Address: "10.0.0.178/24", Interface: "ISP", Dynamic: true},
	}}
	reconciler, storage, request := addressTestSetup(t, []apigen.RouterAddress{}, client)
	if err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(client.writes, []string{"remove"}) || len(client.addresses) != 2 || client.addresses[0].ID != "*management" {
		t.Fatalf("unrelated entries removed: writes=%v addresses=%#v", client.writes, client.addresses)
	}
	assertRouterStatus(t, storage, request.Name, "Ready", "AddressesReconciled")
}

func TestReconcileValidatesTheEntirePlanBeforeCreatingAnything(t *testing.T) {
	client := &addressClient{addresses: []RouterOSAddress{
		{ID: "*foreign", Address: "10.0.4.1/25", Interface: "bridge"},
	}}
	desired := []apigen.RouterAddress{
		testAddress("safe", "10.0.3.1/24", "10.0.3.0"),
		testAddress("conflict", "10.0.4.1/24", "10.0.4.0"),
	}
	reconciler, storage, request := addressTestSetup(t, desired, client)
	if err := reconciler.Reconcile(context.Background(), request); err == nil || len(client.writes) != 0 {
		t.Fatalf("error=%v writes=%v; want refusal before first creation", err, client.writes)
	}
	assertRouterStatus(t, storage, request.Name, "Failed", "AddressOwnershipConflict")
}

// This opt-in test consumes a privately captured read-only snapshot. It never
// connects to a router; even an allowed create is only checked, not executed.
func TestPlanAddressesFromReadOnlySnapshot(t *testing.T) {
	path := os.Getenv("ROUTER_ADDRESS_SNAPSHOT")
	if path == "" {
		t.Skip("no private RouterOS snapshot supplied")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UID        string                 `json:"uid"`
		Management string                 `json:"management"`
		Desired    []apigen.RouterAddress `json:"desired"`
		Existing   []RouterOSAddress      `json:"existing"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	management, err := netip.ParseAddr(fixture.Management)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := planAddresses(fixture.UID, fixture.Desired, fixture.Existing, management)
	if err != nil {
		t.Fatal(err)
	}
	expectedCreate := os.Getenv("ROUTER_ADDRESS_EXPECT_CREATE")
	if expectedCreate == "" {
		if len(changes) != 0 {
			t.Fatalf("expected no writes, got %#v", changes)
		}
		t.Log("Actual planner requires ZERO address writes for this snapshot")
		return
	}
	if len(changes) != 1 || changes[0].action != "create" || changes[0].after.Address != expectedCreate {
		t.Fatalf("expected only creation of %s, got %#v", expectedCreate, changes)
	}
	t.Logf("Actual planner creates only %s; ZERO updates or removals", expectedCreate)
}
