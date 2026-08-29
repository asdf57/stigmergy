package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	apigen "github.com/asdf57/prov-controller-test/go/internal/api/gen"
	"github.com/asdf57/prov-controller-test/go/internal/api/registry"
	"github.com/asdf57/prov-controller-test/go/internal/controller"
	"github.com/asdf57/prov-controller-test/go/internal/resource"
	"github.com/asdf57/prov-controller-test/go/internal/store"
)

type InventoryCaptureGroupReconciler struct {
	store store.Store
}

func NewInventoryCaptureGroupReconciler(store store.Store) *InventoryCaptureGroupReconciler {
	return &InventoryCaptureGroupReconciler{store: store}
}

func (r *InventoryCaptureGroupReconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.InventoryCaptureGroupResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get InventoryCaptureGroup %q: %w", request.Name, err)
	}
	group, err := registry.InventoryCaptureGroupResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode InventoryCaptureGroup %q: %w", request.Name, err)
	}

	selected := make([]resource.Resource, 0)
	requiredLabels := map[string]string{}
	if group.Spec.Selector.MatchLabels != nil {
		requiredLabels = *group.Spec.Selector.MatchLabels
	}
	for _, definition := range CandidateDefinitions() {
		if !kindMatches(definition, group.Spec.Selector.MatchKinds) {
			continue
		}
		resources, err := r.store.List(ctx, definition.Kind)
		if err != nil {
			return fmt.Errorf("list %s resources: %w", definition.Kind, err)
		}
		for _, candidate := range resources.Items {
			if candidate.APIVersion == definition.APIVersion && selectorMatches(candidate.Metadata.Labels, requiredLabels, group.Spec.Selector.MatchExpressions) {
				selected = append(selected, candidate)
			}
		}
	}
	slices.SortFunc(selected, func(left, right resource.Resource) int {
		if left.Kind < right.Kind {
			return -1
		}
		if left.Kind > right.Kind {
			return 1
		}
		if left.Metadata.Name < right.Metadata.Name {
			return -1
		}
		if left.Metadata.Name > right.Metadata.Name {
			return 1
		}
		return 0
	})

	hosts := make(map[string]any, len(selected))
	capturedResources := make([]resource.Resource, 0, len(selected))
	omitted := make([]any, 0)
	nameCounts := make(map[string]int, len(selected))
	for _, candidate := range selected {
		nameCounts[candidate.Metadata.Name]++
	}
	for _, candidate := range selected {
		if nameCounts[candidate.Metadata.Name] > 1 {
			omitted = append(omitted, omittedResource(candidate, "HostNameConflict", "More than one selected resource has this metadata.name"))
			continue
		}
		host, reason, message, ready := captureHost(candidate)
		if !ready {
			omitted = append(omitted, omittedResource(candidate, reason, message))
			continue
		}
		hosts[candidate.Metadata.Name] = host
		capturedResources = append(capturedResources, candidate)
	}

	inventory, configurationErr := buildInventory(group.Spec, capturedResources, hosts)
	status := inventoryStatus(group.Metadata.Generation, len(selected), hosts, omitted, inventory, configurationErr)
	if resource.EqualJSON(group.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(group.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse InventoryCaptureGroup %q resource version: %w", group.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, group.Kind, group.Metadata.Name, status, revision); err != nil {
		return fmt.Errorf("update InventoryCaptureGroup %q status: %w", group.Metadata.Name, err)
	}
	return nil
}

// CandidateDefinitions returns durable resource kinds that can potentially
// expose the canonical management-address status contract. Actual capability
// is determined from each selected object's status during capture.
func CandidateDefinitions() []registry.Definition {
	definitions := make([]registry.Definition, 0, len(registry.Definitions))
	for _, definition := range registry.Definitions {
		if definition.StatusSchema == "" || definition.Kind == registry.InventoryCaptureGroupResource.Kind {
			continue
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

// RequestsForResource reconciles every capture group when a potential source
// resource's labels, FQDN, or management address changes.
func (r *InventoryCaptureGroupReconciler) RequestsForResource(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	groups, err := r.store.List(ctx, registry.InventoryCaptureGroupResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list InventoryCaptureGroups for resource event: %w", err)
	}
	requests := make([]controller.Request, 0, len(groups.Items))
	for _, group := range groups.Items {
		requests = append(requests, controller.Request{Kind: registry.InventoryCaptureGroupResource.Kind, Name: group.Metadata.Name})
	}
	return requests, nil
}

func kindMatches(definition registry.Definition, selectors *[]apigen.InventoryCaptureGroupKindSelector) bool {
	if selectors == nil || len(*selectors) == 0 {
		return true
	}
	for _, selector := range *selectors {
		if selector.ApiVersion == definition.APIVersion && selector.Kind == definition.Kind {
			return true
		}
	}
	return false
}

func resourceKindMatches(candidate resource.Resource, selectors *[]apigen.InventoryCaptureGroupKindSelector) bool {
	if selectors == nil || len(*selectors) == 0 {
		return true
	}
	for _, selector := range *selectors {
		if selector.ApiVersion == candidate.APIVersion && selector.Kind == candidate.Kind {
			return true
		}
	}
	return false
}

func selectorMatches(labels, required map[string]string, expressions *[]apigen.InventoryCaptureGroupMatchExpression) bool {
	for key, value := range required {
		if labels[key] != value {
			return false
		}
	}
	if expressions == nil {
		return true
	}
	for _, expression := range *expressions {
		value, exists := labels[expression.Key]
		values := []string{}
		if expression.Values != nil {
			values = *expression.Values
		}
		contains := slices.Contains(values, value)
		switch string(expression.Operator) {
		case "In":
			if !exists || !contains {
				return false
			}
		case "NotIn":
			if !exists || contains {
				return false
			}
		case "Exists":
			if !exists {
				return false
			}
		case "DoesNotExist":
			if exists {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func omittedResource(candidate resource.Resource, reason, message string) map[string]any {
	return map[string]any{
		"apiVersion": candidate.APIVersion,
		"kind":       candidate.Kind,
		"name":       candidate.Metadata.Name,
		"reason":     reason,
		"message":    message,
	}
}

func captureHost(candidate resource.Resource) (map[string]any, string, string, bool) {
	networking, ok := candidate.Status["networking"].(map[string]any)
	if !ok {
		return nil, "ManagementAddressNotReady", managementConditionMessage(candidate.Status), false
	}
	management, ok := networking["management"].(map[string]any)
	if !ok {
		return nil, "ManagementAddressNotReady", managementConditionMessage(candidate.Status), false
	}
	address, ok := management["address"].(map[string]any)
	if !ok {
		return nil, "ManagementAddressNotReady", managementConditionMessage(candidate.Status), false
	}
	ansibleHost, ok := address["address"].(string)
	if !ok || ansibleHost == "" {
		return nil, "ManagementAddressNotReady", managementConditionMessage(candidate.Status), false
	}
	host := map[string]any{"ansible_host": ansibleHost}
	if fqdn, ok := candidate.Status["fqdn"].(string); ok && fqdn != "" {
		host["fqdn"] = fqdn
	}
	return host, "", "", true
}

func managementConditionMessage(status map[string]any) string {
	conditions, _ := status["conditions"].([]any)
	for _, value := range conditions {
		condition, ok := value.(map[string]any)
		if !ok || condition["type"] != "ManagementAddressReady" {
			continue
		}
		if message, ok := condition["message"].(string); ok && message != "" {
			return message
		}
		if reason, ok := condition["reason"].(string); ok && reason != "" {
			return reason
		}
	}
	return "Server does not have a resolved management address"
}

func buildInventory(spec apigen.InventoryCaptureGroupSpec, captured []resource.Resource, hosts map[string]any) (map[string]any, error) {
	groups := []apigen.InventoryCaptureGroupGroup{}
	if spec.Groups != nil {
		groups = *spec.Groups
	}
	declared := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if group.Name == "all" || group.Name == "ungrouped" {
			return nil, fmt.Errorf("group name %q is reserved by Ansible", group.Name)
		}
		if _, exists := declared[group.Name]; exists {
			return nil, fmt.Errorf("group name %q is declared more than once", group.Name)
		}
		declared[group.Name] = struct{}{}
	}
	groupVars := map[string]map[string]interface{}{}
	if spec.GroupVars != nil {
		groupVars = *spec.GroupVars
	}
	for name := range groupVars {
		if name == "all" {
			continue
		}
		if _, exists := declared[name]; !exists {
			return nil, fmt.Errorf("groupVars %q does not reference a declared group", name)
		}
	}

	inventory := make(map[string]any, len(groups)+1)
	all := map[string]any{"hosts": hosts}
	if variables, exists := groupVars["all"]; exists && len(variables) != 0 {
		all["vars"] = variables
	}
	inventory["all"] = all
	for _, group := range groups {
		requiredLabels := map[string]string{}
		if group.Selector.MatchLabels != nil {
			requiredLabels = *group.Selector.MatchLabels
		}
		groupHosts := make(map[string]any)
		for _, candidate := range captured {
			if resourceKindMatches(candidate, group.Selector.MatchKinds) &&
				selectorMatches(candidate.Metadata.Labels, requiredLabels, group.Selector.MatchExpressions) {
				groupHosts[candidate.Metadata.Name] = hosts[candidate.Metadata.Name]
			}
		}
		capturedGroup := map[string]any{"hosts": groupHosts}
		if variables, exists := groupVars[group.Name]; exists && len(variables) != 0 {
			capturedGroup["vars"] = variables
		}
		inventory[group.Name] = capturedGroup
	}
	return inventory, nil
}

func inventoryStatus(generation int64, matched int, hosts map[string]any, omitted []any, inventory map[string]any, configurationErr error) map[string]any {
	captured := len(hosts)
	phase := "Ready"
	conditionStatus := "True"
	reason := "InventoryCaptured"
	message := fmt.Sprintf("Captured %d resource(s)", captured)
	if configurationErr != nil {
		phase = "Failed"
		conditionStatus = "False"
		reason = "InvalidConfiguration"
		message = configurationErr.Error()
		inventory = nil
	} else if matched == 0 {
		reason = "EmptySelection"
		message = "No resources match the source and selector"
	} else if captured == 0 {
		phase = "Pending"
		conditionStatus = "False"
		reason = "NoResourcesReady"
		message = fmt.Sprintf("All %d matching resource(s) were omitted", matched)
	} else if len(omitted) > 0 {
		phase = "Partial"
		conditionStatus = "False"
		reason = "ResourcesOmitted"
		message = fmt.Sprintf("Captured %d of %d matching resource(s)", captured, matched)
	}
	status := map[string]any{
		"phase":              phase,
		"matchedResources":   matched,
		"capturedResources":  captured,
		"omittedResources":   omitted,
		"observedGeneration": generation,
		"conditions": []any{map[string]any{
			"type":               "Ready",
			"status":             conditionStatus,
			"reason":             reason,
			"message":            message,
			"observedGeneration": generation,
		}},
	}
	if inventory != nil {
		status["inventory"] = inventory
	}
	return status
}
