package webhook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

// MinScaleToZeroIdle is the shortest idle window a MeerkatMob may ask
// for: shorter, an instance would flap between a traversal and the next.
const MinScaleToZeroIdle = time.Minute

// KnowledgeBaseValidator refuses a KnowledgeBase whose parent chain
// loops or that puts it, or anything below it, deeper than the tree
// allows.
type KnowledgeBaseValidator struct {
	// Reader lists the KnowledgeBases in the candidate's namespace.
	Reader client.Reader
}

var _ admission.Validator[*v1alpha1.KnowledgeBase] = (*KnowledgeBaseValidator)(nil)

// ValidateCreate checks a new KnowledgeBase's place in the tree.
func (v *KnowledgeBaseValidator) ValidateCreate(ctx context.Context, kb *v1alpha1.KnowledgeBase) (admission.Warnings, error) {
	return v.validate(ctx, kb)
}

// ValidateUpdate checks the tree again: a new parent can create a loop
// or push the subtree below the KnowledgeBase too deep.
func (v *KnowledgeBaseValidator) ValidateUpdate(ctx context.Context, _, kb *v1alpha1.KnowledgeBase) (admission.Warnings, error) {
	return v.validate(ctx, kb)
}

// ValidateDelete always admits, and warns when children are left
// without a parent.
func (v *KnowledgeBaseValidator) ValidateDelete(ctx context.Context, kb *v1alpha1.KnowledgeBase) (admission.Warnings, error) {
	var list v1alpha1.KnowledgeBaseList
	if err := v.Reader.List(ctx, &list, client.InNamespace(kb.Namespace)); err != nil {
		// Never block a delete on a read failure.
		return admission.Warnings{fmt.Sprintf("could not list KnowledgeBases to check for children: %v", err)}, nil
	}
	if c := childrenOf(kb.Name, list.Items); len(c) > 0 {
		return admission.Warnings{fmt.Sprintf("KnowledgeBases %s name %q as their parent and are left without one", strings.Join(c, ", "), kb.Name)}, nil
	}
	return nil, nil
}

func (v *KnowledgeBaseValidator) validate(ctx context.Context, kb *v1alpha1.KnowledgeBase) (admission.Warnings, error) {
	if err := validateSource(&kb.Spec.Source); err != nil {
		return nil, err
	}
	var list v1alpha1.KnowledgeBaseList
	if err := v.Reader.List(ctx, &list, client.InNamespace(kb.Namespace)); err != nil {
		// Fail closed: the tree cannot be checked, so it is not admitted.
		return nil, fmt.Errorf("listing KnowledgeBases in %q to check the tree: %w", kb.Namespace, err)
	}
	return checkTree(kb, list.Items)
}

// validateSource repeats the CRD's own rules, so that a cluster that
// dropped the CEL validation (an old API server) still refuses them.
func validateSource(s *v1alpha1.KnowledgeBaseSource) error {
	n := 0
	if s.S3 != nil {
		n++
		if strings.HasPrefix(s.S3.Endpoint, "http://") && !s.S3.Insecure {
			return fmt.Errorf("spec.source.s3.endpoint %q is plaintext http; use https, or set insecure: true to accept an unencrypted connection (a local test store, a trusted private network)", s.S3.Endpoint)
		}
	}
	if s.GCS != nil {
		n++
	}
	if s.Bundle != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("spec.source: exactly one of s3, gcs or bundle must be set, got %d", n)
	}
	return nil
}

// MeerkatMobValidator checks a MeerkatMob's own fields and warns when
// its root is missing or is not a tree's root.
type MeerkatMobValidator struct {
	Reader client.Reader
}

var _ admission.Validator[*v1alpha1.MeerkatMob] = (*MeerkatMobValidator)(nil)

// ValidateCreate checks a new MeerkatMob.
func (v *MeerkatMobValidator) ValidateCreate(ctx context.Context, mob *v1alpha1.MeerkatMob) (admission.Warnings, error) {
	return v.validate(ctx, mob)
}

// ValidateUpdate checks a changed MeerkatMob.
func (v *MeerkatMobValidator) ValidateUpdate(ctx context.Context, _, mob *v1alpha1.MeerkatMob) (admission.Warnings, error) {
	return v.validate(ctx, mob)
}

// ValidateDelete always admits.
func (v *MeerkatMobValidator) ValidateDelete(context.Context, *v1alpha1.MeerkatMob) (admission.Warnings, error) {
	return nil, nil
}

func (v *MeerkatMobValidator) validate(ctx context.Context, mob *v1alpha1.MeerkatMob) (admission.Warnings, error) {
	s := &mob.Spec
	if s.ScaleToZero != nil && s.ScaleToZero.Idle != nil && s.ScaleToZero.Idle.Duration < MinScaleToZeroIdle {
		return nil, fmt.Errorf("spec.scaleToZero.idle must be at least %s, got %s", MinScaleToZeroIdle, s.ScaleToZero.Idle.Duration)
	}
	if s.Telemetry != nil && strings.HasPrefix(s.Telemetry.Endpoint, "http://") && !s.Telemetry.Insecure {
		return nil, fmt.Errorf("spec.telemetry.endpoint %q is plaintext http; use https, or set insecure: true", s.Telemetry.Endpoint)
	}
	if s.Librarian != nil {
		if f := strings.Fields(s.Librarian.Schedule); len(f) != 5 {
			return nil, fmt.Errorf("spec.librarian.schedule must be a five-field cron expression, got %q", s.Librarian.Schedule)
		}
	}
	var root v1alpha1.KnowledgeBase
	err := v.Reader.Get(ctx, client.ObjectKey{Namespace: mob.Namespace, Name: s.Root}, &root)
	switch {
	case client.IgnoreNotFound(err) != nil:
		return admission.Warnings{fmt.Sprintf("could not read KnowledgeBase %q: %v", s.Root, err)}, nil
	case err != nil:
		return admission.Warnings{fmt.Sprintf("KnowledgeBase %q does not exist yet: nothing is served until it does", s.Root)}, nil
	case root.Spec.Parent != "":
		return admission.Warnings{fmt.Sprintf("KnowledgeBase %q has a parent (%q): this mob serves only the subtree below it", s.Root, root.Spec.Parent)}, nil
	}
	return nil, nil
}

// Setup registers both validators with mgr's webhook server.
func Setup(mgr manager.Manager) error {
	if err := builder.WebhookManagedBy(mgr, &v1alpha1.KnowledgeBase{}).
		WithValidator(&KnowledgeBaseValidator{Reader: mgr.GetAPIReader()}).
		Complete(); err != nil {
		return fmt.Errorf("KnowledgeBase webhook: %w", err)
	}
	if err := builder.WebhookManagedBy(mgr, &v1alpha1.MeerkatMob{}).
		WithValidator(&MeerkatMobValidator{Reader: mgr.GetAPIReader()}).
		Complete(); err != nil {
		return fmt.Errorf("MeerkatMob webhook: %w", err)
	}
	return nil
}
