package router

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
)

var addressName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type addressError struct {
	reason  string
	message string
}

func (e *addressError) Error() string { return e.message }

func invalidAddresses(reason, message string) error {
	return &addressError{reason: reason, message: message}
}

// addressChange is comparable, so a fresh read can verify the entire planned operation.
type addressChange struct {
	action string
	before RouterOSAddress
	after  RouterOSAddress
}

func addressOwnerPrefix(uid string) string { return "stigmergy:router:" + uid + ":address:" }

func desiredAddress(uid string, desired apigen.RouterAddress) (RouterOSAddress, error) {
	if uid == "" || !addressName.MatchString(desired.Name) || len(desired.Name) > 63 {
		return RouterOSAddress{}, invalidAddresses("InvalidAddresses", "Router UID and unique DNS-style address names are required")
	}
	prefix, err := netip.ParsePrefix(desired.Address)
	if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() {
		return RouterOSAddress{}, invalidAddresses("InvalidAddresses", fmt.Sprintf("address %q requires an IPv4 host address with prefix", desired.Name))
	}
	if prefix.Bits() < 31 && (prefix.Addr() == prefix.Masked().Addr() || !prefix.Contains(prefix.Addr().Next())) {
		return RouterOSAddress{}, invalidAddresses("InvalidAddresses", fmt.Sprintf("address %q cannot use the network or broadcast host address", desired.Name))
	}
	network, err := netip.ParseAddr(desired.Network)
	if err != nil || !network.Is4() || desired.Interface == "" || strings.TrimSpace(desired.Interface) != desired.Interface {
		return RouterOSAddress{}, invalidAddresses("InvalidAddresses", fmt.Sprintf("address %q requires an IPv4 network and interface", desired.Name))
	}
	// RouterOS /32 addressing may explicitly name a different point-to-point peer.
	if prefix.Bits() != 32 && network != prefix.Masked().Addr() {
		return RouterOSAddress{}, invalidAddresses("InvalidAddresses", fmt.Sprintf("address %q network must be %s", desired.Name, prefix.Masked().Addr()))
	}
	return RouterOSAddress{Address: prefix.String(), Network: network.String(),
		Interface: desired.Interface, Comment: addressOwnerPrefix(uid) + desired.Name}, nil
}

func sameAddressSettings(existing, desired RouterOSAddress) bool {
	return existing.Address == desired.Address && existing.Network == desired.Network &&
		existing.Interface == desired.Interface && !existing.Disabled && !existing.Invalid
}

func sameHostAddress(first, second string) bool {
	firstPrefix, firstErr := netip.ParsePrefix(first)
	secondPrefix, secondErr := netip.ParsePrefix(second)
	return firstErr == nil && secondErr == nil && firstPrefix.Addr() == secondPrefix.Addr()
}

// protectManagement also protects a broader connected prefix containing the API IP.
func protectManagement(address RouterOSAddress, management netip.Addr) error {
	prefix, err := netip.ParsePrefix(address.Address)
	if err != nil || prefix.Contains(management) {
		return invalidAddresses("ManagementAddressProtected", fmt.Sprintf("refusing to change address %q that may provide management connectivity", address.Address))
	}
	return nil
}

// planAddresses validates the complete plan before any writes. Existing unowned
// exact matches are satisfied, not adopted; dynamic/unrelated entries survive.
func planAddresses(uid string, desired []apigen.RouterAddress, existing []RouterOSAddress, management netip.Addr) ([]addressChange, error) {
	if uid == "" {
		return nil, invalidAddresses("InvalidAddresses", "Router UID is required for address ownership")
	}
	wanted := make(map[string]RouterOSAddress, len(desired))
	desiredHosts := make(map[string]bool, len(desired))
	ordered := make([]RouterOSAddress, 0, len(desired))
	for _, entry := range desired {
		address, err := desiredAddress(uid, entry)
		if err != nil {
			return nil, err
		}
		prefix, _ := netip.ParsePrefix(address.Address) // Validated by desiredAddress.
		if _, duplicate := wanted[address.Comment]; duplicate || desiredHosts[prefix.Addr().String()] {
			return nil, invalidAddresses("InvalidAddresses", fmt.Sprintf("duplicate address name or host IP at %q", entry.Name))
		}
		wanted[address.Comment] = address
		desiredHosts[prefix.Addr().String()] = true
		ordered = append(ordered, address)
	}
	owned := make(map[string]RouterOSAddress)
	for _, entry := range existing {
		if !strings.HasPrefix(entry.Comment, addressOwnerPrefix(uid)) {
			continue
		}
		if _, duplicate := owned[entry.Comment]; duplicate || entry.ID == "" {
			return nil, invalidAddresses("AddressOwnershipConflict", fmt.Sprintf("duplicate ownership or missing RouterOS ID for %q", entry.Comment))
		}
		owned[entry.Comment] = entry
	}
	var creates, updates, removals []addressChange
	for _, address := range ordered {
		change, err := planDesiredAddress(address, owned, existing, management)
		if err != nil {
			return nil, err
		}
		switch change.action {
		case "create":
			creates = append(creates, change)
		case "update":
			updates = append(updates, change)
		}
	}
	for _, entry := range existing {
		if !strings.HasPrefix(entry.Comment, addressOwnerPrefix(uid)) || entry.Dynamic {
			continue
		}
		if _, retained := wanted[entry.Comment]; retained {
			continue
		}
		if err := protectManagement(entry, management); err != nil {
			return nil, err
		}
		removals = append(removals, addressChange{action: "remove", before: entry})
	}
	return append(append(creates, updates...), removals...), nil
}

func planDesiredAddress(desired RouterOSAddress, owned map[string]RouterOSAddress, existing []RouterOSAddress, management netip.Addr) (addressChange, error) {
	current, found := owned[desired.Comment]
	matchedUnowned := false
	for _, entry := range existing {
		if found && entry.ID == current.ID || !sameHostAddress(entry.Address, desired.Address) {
			continue
		}
		if !found && !entry.Dynamic && !strings.HasPrefix(entry.Comment, "stigmergy:router:") && sameAddressSettings(entry, desired) && !matchedUnowned {
			matchedUnowned = true
			continue
		}
		return addressChange{}, invalidAddresses("AddressOwnershipConflict", fmt.Sprintf("desired address %q conflicts with existing entry %q; no automatic takeover", desired.Address, entry.ID))
	}
	if !found && matchedUnowned {
		return addressChange{}, nil
	}
	if !found {
		if err := protectManagement(desired, management); err != nil {
			return addressChange{}, err
		}
		return addressChange{action: "create", after: desired}, nil
	}
	if current.Dynamic {
		return addressChange{}, invalidAddresses("AddressOwnershipConflict", fmt.Sprintf("owned address %q is dynamic and will not be changed", current.ID))
	}
	if sameAddressSettings(current, desired) {
		return addressChange{}, nil
	}
	if err := protectManagement(current, management); err != nil {
		return addressChange{}, err
	}
	if err := protectManagement(desired, management); err != nil {
		return addressChange{}, err
	}
	desired.ID = current.ID
	return addressChange{action: "update", before: current, after: desired}, nil
}

func (r *Reconciler) requireCurrentRouter(ctx context.Context, original registry.Router) error {
	current, err := r.store.Get(ctx, registry.RouterResource.Kind, original.Metadata.Name)
	if err != nil {
		return err
	}
	if current.Metadata.UID != original.Metadata.UID || current.Metadata.Generation != original.Metadata.Generation || current.Metadata.DeletionTimestamp != nil {
		return invalidAddresses("RouterChanged", "Router changed or was deleted during address reconciliation")
	}
	return ctx.Err()
}

// reconcileAddresses rechecks desired state and RouterOS ownership before each
// write, applies additions/updates before removals, then verifies actual state.
func (r *Reconciler) reconcileAddresses(ctx context.Context, router registry.Router, client RouterClient, management netip.Addr) error {
	existing, err := client.ListAddresses(ctx)
	if err != nil {
		return fmt.Errorf("list existing addresses: %w", err)
	}
	changes, err := planAddresses(router.Metadata.UID, *router.Spec.Addresses, existing, management)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if err := r.requireCurrentRouter(ctx, router); err != nil {
			return err
		}
		current, err := client.ListAddresses(ctx)
		if err != nil {
			return fmt.Errorf("recheck addresses before mutation: %w", err)
		}
		freshPlan, err := planAddresses(router.Metadata.UID, *router.Spec.Addresses, current, management)
		if err != nil {
			return err
		}
		if !containsChange(freshPlan, change) {
			return invalidAddresses("AddressStateChanged", fmt.Sprintf("RouterOS address state changed before %s; replan on the next pass", change.action))
		}
		if err := r.requireCurrentRouter(ctx, router); err != nil {
			return err
		}
		if err := applyAddressChange(ctx, client, change); err != nil {
			return err
		}
	}
	if err := r.requireCurrentRouter(ctx, router); err != nil {
		return err
	}
	observed, err := client.ListAddresses(ctx)
	if err != nil {
		return fmt.Errorf("verify addresses: %w", err)
	}
	remaining, err := planAddresses(router.Metadata.UID, *router.Spec.Addresses, observed, management)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return invalidAddresses("AddressVerificationFailed", fmt.Sprintf("RouterOS still requires %d address changes after reconciliation", len(remaining)))
	}
	return nil
}

func containsChange(changes []addressChange, expected addressChange) bool {
	for _, change := range changes {
		if change == expected {
			return true
		}
	}
	return false
}

func applyAddressChange(ctx context.Context, client RouterClient, change addressChange) error {
	var err error
	switch change.action {
	case "create":
		err = client.AddSubnet(ctx, change.after)
	case "update":
		err = client.UpdateSubnet(ctx, change.after)
	case "remove":
		err = client.RemoveSubnet(ctx, change.before.ID)
	default:
		return invalidAddresses("InvalidAddressPlan", "unknown address mutation")
	}
	if err != nil {
		return invalidAddresses("AddressMutationFailed", fmt.Sprintf("%s address failed: %v", change.action, err))
	}
	return nil
}

func (r *Reconciler) addressFailure(ctx context.Context, router registry.Router, err error) error {
	reason := "AddressReconciliationFailed"
	var failure *addressError
	if errors.As(err, &failure) {
		reason = failure.reason
	}
	if statusErr := r.updateStatus(ctx, router, routerStatus(router, apigen.RouterStatusPhaseFailed, reason, err.Error())); statusErr != nil {
		return errors.Join(err, statusErr)
	}
	return err
}
