package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const (
	routerOSAPIPort = "8728"
	probeTimeout    = 10 * time.Second
)

type Reconciler struct {
	store  store.Store
	prober APIProber
}

func NewReconciler(resourceStore store.Store) *Reconciler {
	return NewReconcilerWithProber(resourceStore, routerOSAPIProber{})
}

func NewReconcilerWithProber(resourceStore store.Store, prober APIProber) *Reconciler {
	return &Reconciler{store: resourceStore, prober: prober}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.RouterResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Router %q: %w", request.Name, err)
	}
	routerResource, err := registry.RouterResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode Router %q: %w", request.Name, err)
	}

	if routerResource.Spec.Type != apigen.RouterTypeMikrotik {
		return r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhaseFailed,
			"UnsupportedRouterType", fmt.Sprintf("router type %q is not supported", routerResource.Spec.Type)))
	}
	if routerResource.Spec.Authentication.Type != apigen.RouterAuthenticationTypeUsernamePasswordCredential {
		return r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhaseFailed,
			"UnsupportedAuthenticationType", fmt.Sprintf("authentication type %q is not supported", routerResource.Spec.Authentication.Type)))
	}

	credentialRaw, err := r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, routerResource.Spec.Authentication.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhasePending,
			"AuthenticationNotFound", fmt.Sprintf("UsernamePasswordCredential %q does not exist", routerResource.Spec.Authentication.Name)))
	}
	if err != nil {
		return fmt.Errorf("get UsernamePasswordCredential %q for Router %q: %w", routerResource.Spec.Authentication.Name, routerResource.Metadata.Name, err)
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(credentialRaw)
	if err != nil {
		return r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhaseFailed,
			"AuthenticationInvalid", fmt.Sprintf("UsernamePasswordCredential %q is invalid: %v", routerResource.Spec.Authentication.Name, err)))
	}

	if !credentialReady(credential) {
		phase, reason := apigen.RouterStatusPhasePending, "AuthenticationNotReady"
		if credentialFailed(credential) {
			phase, reason = apigen.RouterStatusPhaseFailed, "AuthenticationFailed"
		}
		return r.updateStatus(ctx, routerResource, routerStatus(routerResource, phase, reason,
			fmt.Sprintf("UsernamePasswordCredential %q is not Ready for its current generation", credential.Metadata.Name)))
	}

	address, err := netip.ParseAddr(routerResource.Spec.MgmtAddr)
	if err != nil {
		return r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhaseFailed,
			"InvalidManagementAddress", fmt.Sprintf("management address %q is not an IP address", routerResource.Spec.MgmtAddr)))
	}

	probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	endpoint := net.JoinHostPort(address.String(), routerOSAPIPort)
	if err := r.prober.Probe(probeContext, endpoint, credential.Spec.Username, credential.Spec.Password); err != nil {
		message := fmt.Sprintf("RouterOS API at %s is not reachable: %v", endpoint, err)
		if statusErr := r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhaseFailed, "APIUnreachable", message)); statusErr != nil {
			return statusErr
		}
		return fmt.Errorf("probe Router %q API: %w", routerResource.Metadata.Name, err)
	}

	return r.updateStatus(ctx, routerResource, routerStatus(routerResource, apigen.RouterStatusPhaseReady,
		"APIReachable", "RouterOS API accepted an authenticated command"))
}

func credentialReady(credential registry.UsernamePasswordCredential) bool {
	return credential.Status != nil && credential.Status.Phase != nil &&
		*credential.Status.Phase == apigen.UsernamePasswordCredentialStatusPhaseReady &&
		credential.Status.ObservedGeneration != nil &&
		*credential.Status.ObservedGeneration == credential.Metadata.Generation
}

func credentialFailed(credential registry.UsernamePasswordCredential) bool {
	return credential.Status != nil && credential.Status.Phase != nil &&
		(*credential.Status.Phase == apigen.UsernamePasswordCredentialStatusPhaseFailed ||
			*credential.Status.Phase == apigen.UsernamePasswordCredentialStatusPhaseConflict) &&
		credential.Status.ObservedGeneration != nil &&
		*credential.Status.ObservedGeneration == credential.Metadata.Generation
}

func routerStatus(routerResource registry.Router, phase apigen.RouterStatusPhase, reason, message string) *apigen.RouterStatus {
	observedGeneration := routerResource.Metadata.Generation
	conditionStatus := apigen.RouterConditionStatusFalse
	if phase == apigen.RouterStatusPhaseReady {
		conditionStatus = apigen.RouterConditionStatusTrue
	}
	conditions := []apigen.RouterCondition{{
		Type: "Ready", Status: conditionStatus, Reason: reason,
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	return &apigen.RouterStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration, Conditions: &conditions,
	}
}

func (r *Reconciler) updateStatus(ctx context.Context, routerResource registry.Router, status *apigen.RouterStatus) error {
	if resource.EqualJSON(routerResource.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(routerResource.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse Router %q resource version: %w", routerResource.Metadata.Name, err)
	}
	storedStatus, err := registry.RouterResource.EncodeStatus(status)
	if err != nil {
		return fmt.Errorf("encode Router %q status: %w", routerResource.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, routerResource.Kind, routerResource.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update Router %q status: %w", routerResource.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) RequestsForCredential(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	routers, err := r.store.List(ctx, registry.RouterResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list Routers for UsernamePasswordCredential %q: %w", request.Name, err)
	}
	requests := make([]controller.Request, 0)
	for _, raw := range routers.Items {
		routerResource, err := registry.RouterResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode Router %q while mapping UsernamePasswordCredential %q: %w", raw.Metadata.Name, request.Name, err)
		}
		if routerResource.Spec.Authentication.Type == apigen.RouterAuthenticationTypeUsernamePasswordCredential &&
			routerResource.Spec.Authentication.Name == request.Name {
			requests = append(requests, controller.Request{Kind: registry.RouterResource.Kind, Name: routerResource.Metadata.Name})
		}
	}
	return requests, nil
}
