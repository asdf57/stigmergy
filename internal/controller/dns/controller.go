package dns

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

const (
	cleanupFinalizer   = "homelab.io/dns-record-cleanup"
	programmingTimeout = 10 * time.Second
)

type Reconciler struct {
	store   store.Store
	backend Backend
}

func NewReconciler(resourceStore store.Store) *Reconciler {
	return NewReconcilerWithBackend(resourceStore, routerOSBackend{})
}

func NewReconcilerWithBackend(resourceStore store.Store, backend Backend) *Reconciler {
	return &Reconciler{store: resourceStore, backend: backend}
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, registry.DNSRecordResource.Kind, request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get DNSRecord %q: %w", request.Name, err)
	}
	record, err := registry.DNSRecordResource.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode DNSRecord %q: %w", request.Name, err)
	}
	if record.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, record)
	}

	if record.Spec.BackingStoreRef.Kind != registry.RouterResource.Kind {
		return r.updateStatus(ctx, record, recordStatus(record, nil, apigen.DNSRecordStatusPhaseFailed,
			"UnsupportedBackingStoreKind", fmt.Sprintf("backing store kind %q is not supported", record.Spec.BackingStoreRef.Kind)))
	}

	routerResource, status, err := r.resolveRouter(ctx, record)
	if status != nil {
		return r.updateStatus(ctx, record, status)
	}
	if err != nil {
		return err
	}

	credential, status, err := r.resolveCredential(ctx, record, routerResource)
	if status != nil {
		return r.updateStatus(ctx, record, status)
	}
	if err != nil {
		return err
	}

	desired, err := desiredRecord(record)
	if errors.Is(err, errUnsupportedRecordType) {
		return r.updateStatus(ctx, record, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhaseFailed,
			"UnsupportedRecordType", err.Error()))
	}
	if err != nil {
		return r.updateStatus(ctx, record, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhaseFailed,
			"InvalidRecordValue", err.Error()))
	}
	connection := Connection{
		ManagementAddress: routerResource.Spec.MgmtAddr,
		Username:          credential.Spec.Username,
		Password:          credential.Spec.Password,
	}
	programmingContext, cancel := context.WithTimeout(ctx, programmingTimeout)
	defer cancel()
	if err := r.backend.Ensure(programmingContext, connection, desired); err != nil {
		message := fmt.Sprintf("program DNS record through Router %q: %v", routerResource.Metadata.Name, err)
		if statusErr := r.updateStatus(ctx, record, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhaseFailed,
			"ProgrammingFailed", message)); statusErr != nil {
			return statusErr
		}
		return fmt.Errorf("program DNSRecord %q: %w", record.Metadata.Name, err)
	}

	return r.updateStatus(ctx, record, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhaseReady,
		"RecordProgrammed", fmt.Sprintf("DNS record is programmed through Router %q", routerResource.Metadata.Name)))
}

func (r *Reconciler) finalize(ctx context.Context, record registry.DNSRecord) error {
	if !slices.Contains(record.Metadata.Finalizers, cleanupFinalizer) {
		return nil
	}
	if record.Spec.BackingStoreRef.Kind != registry.RouterResource.Kind {
		return r.removeFinalizer(ctx, record)
	}

	rawRouter, err := r.store.Get(ctx, registry.RouterResource.Kind, record.Spec.BackingStoreRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return r.removeFinalizer(ctx, record)
	}
	if err != nil {
		return fmt.Errorf("get Router %q while finalizing DNSRecord %q: %w", record.Spec.BackingStoreRef.Name, record.Metadata.Name, err)
	}
	routerResource, err := registry.RouterResource.Decode(rawRouter)
	if err != nil {
		return fmt.Errorf("decode Router %q while finalizing DNSRecord %q: %w", record.Spec.BackingStoreRef.Name, record.Metadata.Name, err)
	}
	if routerResource.Spec.Authentication.Type != apigen.RouterAuthenticationTypeUsernamePasswordCredential {
		return r.removeFinalizer(ctx, record)
	}

	credentialName := routerResource.Spec.Authentication.Name
	rawCredential, err := r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, credentialName)
	if errors.Is(err, store.ErrNotFound) {
		return r.removeFinalizer(ctx, record)
	}
	if err != nil {
		return fmt.Errorf("get UsernamePasswordCredential %q while finalizing DNSRecord %q: %w", credentialName, record.Metadata.Name, err)
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(rawCredential)
	if err != nil {
		return fmt.Errorf("decode UsernamePasswordCredential %q while finalizing DNSRecord %q: %w", credentialName, record.Metadata.Name, err)
	}

	connection := Connection{
		ManagementAddress: routerResource.Spec.MgmtAddr,
		Username:          credential.Spec.Username,
		Password:          credential.Spec.Password,
	}
	cleanupContext, cancel := context.WithTimeout(ctx, programmingTimeout)
	defer cancel()
	if err := r.backend.Delete(cleanupContext, connection, record.Metadata.UID); err != nil {
		return fmt.Errorf("delete RouterOS entry for DNSRecord %q: %w", record.Metadata.Name, err)
	}
	return r.removeFinalizer(ctx, record)
}

func (r *Reconciler) removeFinalizer(ctx context.Context, record registry.DNSRecord) error {
	record.Metadata.Finalizers = slices.DeleteFunc(record.Metadata.Finalizers, func(value string) bool {
		return value == cleanupFinalizer
	})
	raw, err := record.Encode()
	if err != nil {
		return fmt.Errorf("encode DNSRecord %q while removing finalizer: %w", record.Metadata.Name, err)
	}
	revision, err := strconv.ParseInt(record.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse DNSRecord %q resource version while removing finalizer: %w", record.Metadata.Name, err)
	}
	if _, err := r.store.Update(ctx, raw, revision); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove finalizer from DNSRecord %q: %w", record.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) resolveRouter(ctx context.Context, record registry.DNSRecord) (registry.Router, *apigen.DNSRecordStatus, error) {
	name := record.Spec.BackingStoreRef.Name
	raw, err := r.store.Get(ctx, registry.RouterResource.Kind, name)
	if errors.Is(err, store.ErrNotFound) {
		return registry.Router{}, recordStatus(record, nil, apigen.DNSRecordStatusPhasePending,
			"BackingStoreNotFound", fmt.Sprintf("Router %q does not exist", name)), nil
	}
	if err != nil {
		return registry.Router{}, nil, fmt.Errorf("get Router %q for DNSRecord %q: %w", name, record.Metadata.Name, err)
	}
	routerResource, err := registry.RouterResource.Decode(raw)
	if err != nil {
		return registry.Router{}, recordStatus(record, nil, apigen.DNSRecordStatusPhaseFailed,
			"BackingStoreInvalid", fmt.Sprintf("Router %q is invalid: %v", name, err)), nil
	}
	if !routerReady(routerResource) {
		phase, reason := apigen.DNSRecordStatusPhasePending, "BackingStoreNotReady"
		if routerFailed(routerResource) {
			phase, reason = apigen.DNSRecordStatusPhaseFailed, "BackingStoreFailed"
		}
		return registry.Router{}, recordStatus(record, &routerResource, phase, reason,
			fmt.Sprintf("Router %q is not Ready for its current generation", name)), nil
	}
	return routerResource, nil, nil
}

func (r *Reconciler) resolveCredential(ctx context.Context, record registry.DNSRecord, routerResource registry.Router) (registry.UsernamePasswordCredential, *apigen.DNSRecordStatus, error) {
	if routerResource.Spec.Authentication.Type != apigen.RouterAuthenticationTypeUsernamePasswordCredential {
		return registry.UsernamePasswordCredential{}, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhaseFailed,
			"UnsupportedAuthenticationType", fmt.Sprintf("Router authentication type %q is not supported", routerResource.Spec.Authentication.Type)), nil
	}
	name := routerResource.Spec.Authentication.Name
	raw, err := r.store.Get(ctx, registry.UsernamePasswordCredentialResource.Kind, name)
	if errors.Is(err, store.ErrNotFound) {
		return registry.UsernamePasswordCredential{}, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhasePending,
			"AuthenticationNotFound", fmt.Sprintf("UsernamePasswordCredential %q does not exist", name)), nil
	}
	if err != nil {
		return registry.UsernamePasswordCredential{}, nil, fmt.Errorf("get UsernamePasswordCredential %q for DNSRecord %q: %w", name, record.Metadata.Name, err)
	}
	credential, err := registry.UsernamePasswordCredentialResource.Decode(raw)
	if err != nil {
		return registry.UsernamePasswordCredential{}, recordStatus(record, &routerResource, apigen.DNSRecordStatusPhaseFailed,
			"AuthenticationInvalid", fmt.Sprintf("UsernamePasswordCredential %q is invalid: %v", name, err)), nil
	}
	if !credentialReady(credential) {
		phase, reason := apigen.DNSRecordStatusPhasePending, "AuthenticationNotReady"
		if credentialFailed(credential) {
			phase, reason = apigen.DNSRecordStatusPhaseFailed, "AuthenticationFailed"
		}
		return registry.UsernamePasswordCredential{}, recordStatus(record, &routerResource, phase, reason,
			fmt.Sprintf("UsernamePasswordCredential %q is not Ready for its current generation", name)), nil
	}
	return credential, nil, nil
}

func routerReady(routerResource registry.Router) bool {
	return routerResource.Status != nil && routerResource.Status.Phase != nil &&
		*routerResource.Status.Phase == apigen.RouterStatusPhaseReady &&
		routerResource.Status.ObservedGeneration != nil &&
		*routerResource.Status.ObservedGeneration == routerResource.Metadata.Generation
}

func routerFailed(routerResource registry.Router) bool {
	return routerResource.Status != nil && routerResource.Status.Phase != nil &&
		*routerResource.Status.Phase == apigen.RouterStatusPhaseFailed &&
		routerResource.Status.ObservedGeneration != nil &&
		*routerResource.Status.ObservedGeneration == routerResource.Metadata.Generation
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

func recordStatus(record registry.DNSRecord, routerResource *registry.Router, phase apigen.DNSRecordStatusPhase, reason, message string) *apigen.DNSRecordStatus {
	observedGeneration := record.Metadata.Generation
	conditionStatus := apigen.DNSRecordConditionStatusFalse
	if phase == apigen.DNSRecordStatusPhaseReady {
		conditionStatus = apigen.DNSRecordConditionStatusTrue
	}
	conditions := []apigen.DNSRecordCondition{{
		Type: "Ready", Status: conditionStatus, Reason: reason,
		Message: &message, ObservedGeneration: &observedGeneration,
	}}
	status := &apigen.DNSRecordStatus{
		Phase: &phase, ObservedGeneration: &observedGeneration, Conditions: &conditions,
	}
	if routerResource != nil {
		status.BackingStore = &apigen.DNSRecordBackingStoreStatus{
			ApiVersion:         routerResource.APIVersion,
			Kind:               routerResource.Kind,
			Name:               routerResource.Metadata.Name,
			Uid:                routerResource.Metadata.UID,
			ObservedGeneration: routerResource.Metadata.Generation,
		}
	}
	return status
}

func (r *Reconciler) updateStatus(ctx context.Context, record registry.DNSRecord, status *apigen.DNSRecordStatus) error {
	if resource.EqualJSON(record.Status, status) {
		return nil
	}
	revision, err := strconv.ParseInt(record.Metadata.ResourceVersion, 10, 64)
	if err != nil {
		return fmt.Errorf("parse DNSRecord %q resource version: %w", record.Metadata.Name, err)
	}
	storedStatus, err := registry.DNSRecordResource.EncodeStatus(status)
	if err != nil {
		return fmt.Errorf("encode DNSRecord %q status: %w", record.Metadata.Name, err)
	}
	if _, err := r.store.UpdateStatus(ctx, record.Kind, record.Metadata.Name, storedStatus, revision); err != nil {
		return fmt.Errorf("update DNSRecord %q status: %w", record.Metadata.Name, err)
	}
	return nil
}

func (r *Reconciler) RequestsForRouter(ctx context.Context, request controller.Request) ([]controller.Request, error) {
	records, err := r.store.List(ctx, registry.DNSRecordResource.Kind)
	if err != nil {
		return nil, fmt.Errorf("list DNSRecords for Router %q: %w", request.Name, err)
	}
	requests := make([]controller.Request, 0)
	for _, raw := range records.Items {
		record, err := registry.DNSRecordResource.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode DNSRecord %q while mapping Router %q: %w", raw.Metadata.Name, request.Name, err)
		}
		if record.Spec.BackingStoreRef.Kind == registry.RouterResource.Kind && record.Spec.BackingStoreRef.Name == request.Name {
			requests = append(requests, controller.Request{Kind: registry.DNSRecordResource.Kind, Name: record.Metadata.Name})
		}
	}
	return requests, nil
}
