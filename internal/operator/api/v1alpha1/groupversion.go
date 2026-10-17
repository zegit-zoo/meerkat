// Package v1alpha1 holds the meerkat operator's custom resources: a
// KnowledgeBase is one node of a knowledge-base tree, and a MeerkatMob
// is the set of meerkat instances that serve a tree from its root
// (meerkat-mob#10, phase 2).
//
// The types mirror the `tree:` manifest in internal/contentsource: a
// KnowledgeBase's parent, mount, placement, access and depth limit mean
// what the same keys mean in a manifest, and the validating webhook in
// internal/operator/webhook applies the manifest loader's depth and
// cycle rules at admission.
//
// +kubebuilder:object:generate=true
// +groupName=meerkat-mob.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version of these resources.
	GroupVersion = schema.GroupVersion{Group: "meerkat-mob.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a runtime.Scheme. Only
	// apimachinery is imported, so the package stays cheap to import.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&KnowledgeBase{}, &KnowledgeBaseList{},
		&MeerkatMob{}, &MeerkatMobList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
