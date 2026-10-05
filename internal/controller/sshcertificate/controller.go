// Package sshcertificate manages short-lived OpenSSH user certificates.
// Only privileged operators may create these resources: principals are signing authority.
package sshcertificate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	apigen "github.com/asdf57/stigmergy/internal/api/gen"
	"github.com/asdf57/stigmergy/internal/api/registry"
	"github.com/asdf57/stigmergy/internal/controller"
	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
	"golang.org/x/crypto/ssh"
)

const finalizer = "homelab.io/ssh-certificate-cleanup"
const owner = "homelab.io/ssh-certificate-uid"
const inputDigest = "homelab.io/ssh-certificate-input-digest"

type Reconciler struct {
	store store.Store
	now   func() time.Time
}

func NewReconciler(s store.Store) *Reconciler { return &Reconciler{store: s, now: time.Now} }
func ready(value resource.Resource) bool {
	return value.Metadata.DeletionTimestamp == nil && value.Status["phase"] == "Ready" && resource.EqualJSON(value.Status["observedGeneration"], value.Metadata.Generation)
}
func pinned(ref apigen.SSHCertificateReference, previous *apigen.ResourceReference, uid string) bool {
	if ref.Uid != nil {
		return *ref.Uid == uid
	}
	return previous == nil || previous.Name != ref.Name || previous.Uid == uid
}
func revision(value resource.Resource) (int64, error) {
	return strconv.ParseInt(value.Metadata.ResourceVersion, 10, 64)
}

func (r *Reconciler) Reconcile(ctx context.Context, request controller.Request) error {
	raw, err := r.store.Get(ctx, "SSHCertificate", request.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cert, err := registry.SSHCertificateResource.Decode(raw)
	if err != nil {
		return err
	}
	if raw.Metadata.DeletionTimestamp != nil {
		return r.finalize(ctx, raw)
	}
	status := &apigen.SSHCertificateStatus{}
	if cert.Status != nil {
		*status = *cert.Status
	}
	fail := func(phase, reason, message string) error {
		return r.setStatus(ctx, raw, status, phase, reason, message)
	}
	ttl, err := time.ParseDuration(cert.Spec.Ttl)
	if err != nil || ttl < 5*time.Minute || ttl > 7*24*time.Hour {
		return fail("Failed", "InvalidLifetime", "ttl must be between 5 minutes and 7 days")
	}
	renew, err := time.ParseDuration(cert.Spec.RenewBefore)
	if err != nil || renew < time.Minute || renew >= ttl {
		return fail("Failed", "InvalidRenewal", "renewBefore must be at least one minute and less than ttl")
	}
	principalPattern := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@-]{0,127}$`)
	principals := slices.Clone(cert.Spec.Principals)
	slices.Sort(principals)
	if len(principals) == 0 || len(principals) > 32 || len(slices.Compact(slices.Clone(principals))) != len(principals) {
		return fail("Failed", "InvalidPrincipals", "explicit unique principals are required")
	}
	for _, p := range principals {
		if !principalPattern.MatchString(p) {
			return fail("Failed", "InvalidPrincipals", "invalid certificate principal")
		}
	}
	caRaw, err := r.store.Get(ctx, "SSHCertificateAuthority", cert.Spec.AuthorityRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return fail("Pending", "AuthorityMissing", "authority does not exist")
	}
	if err != nil {
		return err
	}
	if !pinned(cert.Spec.AuthorityRef, status.AuthorityRef, caRaw.Metadata.UID) {
		return fail("Conflict", "AuthorityReplaced", "explicitly bind the replacement authority UID")
	}
	if !ready(caRaw) {
		return fail("Pending", "AuthorityNotReady", "authority is not Ready at its current generation")
	}
	ca, err := registry.SSHCertificateAuthorityResource.Decode(caRaw)
	if err != nil {
		return err
	}
	if ca.Status.SigningKeyRef == nil || ca.Status.SigningKeyRef.Name != ca.Spec.SigningKeyRef.Name {
		return fail("Pending", "SignerUnresolved", "authority signer is unresolved")
	}
	keyRaw, err := r.store.Get(ctx, "SSHKeyPair", cert.Spec.KeyPairRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return fail("Pending", "KeyMissing", "subject key does not exist")
	}
	if err != nil {
		return err
	}
	if !pinned(cert.Spec.KeyPairRef, status.KeyPairRef, keyRaw.Metadata.UID) {
		return fail("Conflict", "KeyReplaced", "explicitly bind the replacement subject key UID")
	}
	if !ready(keyRaw) {
		return fail("Pending", "KeyNotReady", "subject key is not Ready")
	}
	key, err := registry.SSHKeyPairResource.Decode(keyRaw)
	if err != nil {
		return err
	}
	if key.Status.PublicKey == nil {
		return fail("Failed", "InvalidSubject", "subject public key is unavailable")
	}
	public, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(*key.Status.PublicKey))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 || public.Type() != ssh.KeyAlgoED25519 {
		return fail("Failed", "InvalidSubject", "a plain Ed25519 subject public key is required")
	}
	signerRaw, err := r.store.Get(ctx, "SSHKeyPair", ca.Status.SigningKeyRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return fail("Pending", "SignerMissing", "signer key is unavailable")
	}
	if err != nil {
		return err
	}
	if !ready(signerRaw) || signerRaw.Metadata.UID != ca.Status.SigningKeyRef.Uid {
		return fail("Pending", "SignerNotReady", "signer identity is not current")
	}
	signerKey, err := registry.SSHKeyPairResource.Decode(signerRaw)
	if err != nil {
		return err
	}
	if signerKey.Status.SecretRef == nil || signerKey.Status.PublicKey == nil {
		return fail("Pending", "SignerSecretMissing", "signer secret is unresolved")
	}
	secretRaw, err := r.store.Get(ctx, "Secret", signerKey.Status.SecretRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return fail("Pending", "SignerSecretMissing", "signer secret is unavailable")
	}
	if err != nil {
		return err
	}
	if !ready(secretRaw) || secretRaw.Metadata.UID != signerKey.Status.SecretRef.Uid || secretRaw.Metadata.Annotations["homelab.io/ssh-key-pair-uid"] != signerRaw.Metadata.UID {
		return fail("Conflict", "SignerSecretChanged", "signer secret identity is not current")
	}
	signerSecret, err := registry.SecretResource.Decode(secretRaw)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey([]byte(signerSecret.Spec.Data["privateKey"]))
	if err != nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != strings.TrimSpace(*signerKey.Status.PublicKey) {
		return fail("Failed", "InvalidSigner", "signer private/public key identity mismatch")
	}
	trusted := false
	if ca.Status.TrustBundle != nil {
		for _, entry := range *ca.Status.TrustBundle {
			if entry.KeyPairRef.Uid == signerRaw.Metadata.UID && strings.TrimSpace(entry.PublicKey) == strings.TrimSpace(*signerKey.Status.PublicKey) {
				trusted = true
			}
		}
	}
	if !trusted {
		return fail("Failed", "SignerNotTrusted", "current signer is absent from the authority trust bundle")
	}
	// The digest excludes unrelated trusted keys: adding rollover trust does not renew
	// an unchanged signer certificate, but changing the signer does.
	inputs, _ := json.Marshal([]any{caRaw.Metadata.UID, signerRaw.Metadata.UID, keyRaw.Metadata.UID, *key.Status.PublicKey, principals, cert.Spec.Ttl, cert.Spec.RenewBefore})
	hash := sha256.Sum256(inputs)
	digest := hex.EncodeToString(hash[:])
	name := "ssh-certificate-" + cert.Metadata.Name
	allSecrets, err := r.store.List(ctx, "Secret")
	if err != nil {
		return err
	}
	for _, candidate := range allSecrets.Items {
		if candidate.Metadata.Name == name {
			continue
		}
		other, err := registry.SecretResource.Decode(candidate)
		if err != nil {
			return err
		}
		if other.Spec.SecretStoreRef.Name == cert.Spec.SecretStoreRef.Name && other.Spec.Path == cert.Spec.Path {
			return fail("Conflict", "OutputPathConflict", "certificate output path is already managed by another Secret")
		}
	}
	out, err := r.store.Get(ctx, "Secret", name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	exists := err == nil
	if exists {
		prior, err := registry.SecretResource.Decode(out)
		if err != nil {
			return err
		}
		if prior.Spec.SecretStoreRef.Name != cert.Spec.SecretStoreRef.Name || prior.Spec.Path != cert.Spec.Path {
			return fail("Conflict", "OutputLocationChanged", "create a new certificate resource to change its external output location")
		}
	}
	if exists && (out.Metadata.DeletionTimestamp != nil || out.Metadata.Annotations[owner] != raw.Metadata.UID || (status.SecretRef != nil && status.SecretRef.Uid != out.Metadata.UID)) {
		return fail("Conflict", "OutputOwnershipConflict", "certificate Secret is not owned by this resource lifetime")
	}
	var issued *ssh.Certificate
	var value string
	if exists && out.Metadata.Annotations[inputDigest] == digest {
		output, err := registry.SecretResource.Decode(out)
		if err != nil {
			return err
		}
		value = output.Spec.Data["value"]
		parsed, _, _, trailing, err := ssh.ParseAuthorizedKey([]byte(value))
		if err == nil && len(bytes.TrimSpace(trailing)) == 0 {
			candidate, ok := parsed.(*ssh.Certificate)
			if ok && candidate.CertType == ssh.UserCert && bytes.Equal(candidate.Key.Marshal(), public.Marshal()) && bytes.Equal(candidate.SignatureKey.Marshal(), signer.PublicKey().Marshal()) && slices.Equal(candidate.ValidPrincipals, principals) && candidate.KeyId == raw.Metadata.UID && candidate.ValidBefore > uint64(r.now().Add(renew).Unix()) && candidate.ValidAfter <= uint64(r.now().Unix()) {
				checker := ssh.CertChecker{Clock: r.now}
				if checker.CheckCert(principals[0], candidate) == nil {
					issued = candidate
				}
			}
		}
	}
	if issued == nil {
		serialBytes := make([]byte, 8)
		if _, err := rand.Read(serialBytes); err != nil {
			return err
		}
		issued = &ssh.Certificate{Key: public, Serial: binary.BigEndian.Uint64(serialBytes), CertType: ssh.UserCert, KeyId: raw.Metadata.UID, ValidPrincipals: principals, ValidAfter: uint64(r.now().Add(-time.Minute).Unix()), ValidBefore: uint64(r.now().Add(ttl).Unix()), Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}}}
		if err := issued.SignCert(rand.Reader, signer); err != nil {
			return fail("Failed", "SigningFailed", "unable to sign certificate")
		}
		value = string(ssh.MarshalAuthorizedKey(issued))
	}
	desired := registry.NewSecret(resource.Metadata{Name: name, Annotations: map[string]string{owner: raw.Metadata.UID, inputDigest: digest}, Finalizers: []string{"homelab.io/secret-cleanup"}}, apigen.SecretSpec{SecretStoreRef: apigen.SecretStoreReference{Name: cert.Spec.SecretStoreRef.Name}, Path: cert.Spec.Path, Data: map[string]string{"value": value}})
	if exists {
		encoded, err := desired.Encode()
		if err != nil {
			return err
		}
		if !resource.EqualJSON(out.Spec, encoded.Spec) || out.Metadata.Annotations[inputDigest] != digest {
			out.Spec = encoded.Spec
			out.Metadata.Annotations[inputDigest] = digest
			version, err := revision(out)
			if err != nil {
				return err
			}
			out, err = r.store.Update(ctx, out, version)
			if err != nil {
				return err
			}
		}
	} else {
		encoded, err := desired.Encode()
		if err != nil {
			return err
		}
		out, err = r.store.Create(ctx, encoded)
		if err != nil {
			return err
		}
	}
	status.AuthorityRef = &apigen.ResourceReference{Name: caRaw.Metadata.Name, Uid: caRaw.Metadata.UID}
	status.KeyPairRef = &apigen.ResourceReference{Name: keyRaw.Metadata.Name, Uid: keyRaw.Metadata.UID}
	status.SigningKeyRef = &apigen.ResourceReference{Name: signerRaw.Metadata.Name, Uid: signerRaw.Metadata.UID}
	status.SecretRef = &apigen.ResourceReference{Name: out.Metadata.Name, Uid: out.Metadata.UID}
	serial := strconv.FormatUint(issued.Serial, 10)
	after, before := time.Unix(int64(issued.ValidAfter), 0).UTC(), time.Unix(int64(issued.ValidBefore), 0).UTC()
	renewAt := before.Add(-renew)
	status.Certificate, status.Serial, status.ValidAfter, status.ValidBefore, status.RenewAt = &value, &serial, &after, &before, &renewAt
	if !ready(out) {
		return fail("Pending", "SecretPending", "public certificate is awaiting external Secret publication")
	}
	return r.setStatus(ctx, raw, status, "Ready", "CertificatePublished", "user certificate is published; renewal is automatic")
}

func (r *Reconciler) setStatus(ctx context.Context, raw resource.Resource, status *apigen.SSHCertificateStatus, phase, reason, message string) error {
	p := apigen.SSHCertificateStatusPhase(phase)
	status.Phase, status.Reason, status.Message, status.ObservedGeneration = &p, &reason, &message, &raw.Metadata.Generation
	encoded, err := registry.SSHCertificateResource.EncodeStatus(status)
	if err != nil {
		return err
	}
	if resource.EqualJSON(raw.Status, encoded) {
		return nil
	}
	version, err := revision(raw)
	if err != nil {
		return err
	}
	_, err = r.store.UpdateStatus(ctx, raw.Kind, raw.Metadata.Name, encoded, version)
	return err
}

func (r *Reconciler) finalize(ctx context.Context, raw resource.Resource) error {
	if !slices.Contains(raw.Metadata.Finalizers, finalizer) {
		return nil
	}
	out, err := r.store.Get(ctx, "Secret", "ssh-certificate-"+raw.Metadata.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil {
		if out.Metadata.Annotations[owner] != raw.Metadata.UID {
			return fmt.Errorf("refusing to delete another owner's certificate Secret")
		}
		if out.Metadata.DeletionTimestamp == nil {
			version, err := revision(out)
			if err != nil {
				return err
			}
			return r.store.Delete(ctx, out.Kind, out.Metadata.Name, version)
		}
		return fmt.Errorf("waiting for external certificate Secret cleanup")
	}
	raw.Metadata.Finalizers = slices.DeleteFunc(raw.Metadata.Finalizers, func(value string) bool { return value == finalizer })
	version, err := revision(raw)
	if err != nil {
		return err
	}
	_, err = r.store.Update(ctx, raw, version)
	return err
}

// Dependency updates are infrequent; requeue managed certificates immediately.
func (r *Reconciler) RequestsForDependency(ctx context.Context, _ controller.Request) ([]controller.Request, error) {
	values, err := r.store.List(ctx, "SSHCertificate")
	if err != nil {
		return nil, err
	}
	requests := make([]controller.Request, 0, len(values.Items))
	for _, value := range values.Items {
		requests = append(requests, controller.Request{Kind: value.Kind, Name: value.Metadata.Name})
	}
	return requests, nil
}
