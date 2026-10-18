package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mount modes, as in a tree manifest's children.
const (
	MountEager = "eager"
	MountLazy  = "lazy"
)

// Placements, as in a tree manifest. A dedicated KnowledgeBase always
// gets its own Deployment and NetworkPolicy, whatever the mob's
// placement says.
const (
	PlacementShared    = "shared"
	PlacementDedicated = "dedicated"
)

// KnowledgeBaseSpec is where one knowledge base's content lives and
// where it sits in the tree.
type KnowledgeBaseSpec struct {
	// Source is where the content lives: exactly one of s3, gcs or
	// bundle.
	Source KnowledgeBaseSource `json:"source"`

	// Parent names the KnowledgeBase, in the same namespace, that this
	// one hangs under. Empty for a tree's root. The webhook refuses a
	// parent chain that loops or that puts this KnowledgeBase, or any
	// below it, deeper than five levels (root = 0). It must be a
	// Kubernetes object name (a DNS-1123 subdomain), or empty: blanking
	// the value in kubectl edit makes this KnowledgeBase a root again.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Parent string `json:"parent,omitempty"`

	// Tier groups KnowledgeBases for a MeerkatMob with placement:
	// perTier. 0 is the hot, fast surface.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	Tier int32 `json:"tier,omitempty"`

	// Mount is eager (the default) or lazy: a lazy KnowledgeBase is
	// mounted on its first traversal.
	// +optional
	// +kubebuilder:default=eager
	// +kubebuilder:validation:Enum=eager;lazy
	Mount string `json:"mount,omitempty"`

	// Placement is shared (the default) or dedicated.
	// +optional
	// +kubebuilder:default=shared
	// +kubebuilder:validation:Enum=shared;dedicated
	Placement string `json:"placement,omitempty"`

	// Access restricts who may reach this KnowledgeBase and by which
	// tree path. It maps to the hosted server's authz rules.
	// +optional
	Access *Access `json:"access,omitempty"`

	// DepthLimit may lower the five-level cap for the subtree below
	// this KnowledgeBase, never raise it: a child may sit at most
	// DepthLimit levels below this one.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	DepthLimit *int32 `json:"depthLimit,omitempty"`

	// Resources bound what this KnowledgeBase may hold resident.
	// +optional
	Resources *KnowledgeBaseResources `json:"resources,omitempty"`

	// Refresh makes an object-store source follow new versions instead
	// of serving the one it started on.
	// +optional
	Refresh *Refresh `json:"refresh,omitempty"`

	// Contract is the update contract shown to callers, as a tree
	// manifest's contract: a short description of how this knowledge
	// base may be changed.
	// +optional
	// +kubebuilder:validation:MaxLength=2000
	Contract string `json:"contract,omitempty"`

	// Description is shown by mk_list_collections.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Description string `json:"description,omitempty"`
}

// KnowledgeBaseSource is exactly one content location.
// +kubebuilder:validation:XValidation:rule="(has(self.s3) ? 1 : 0) + (has(self.gcs) ? 1 : 0) + (has(self.bundle) ? 1 : 0) == 1",message="exactly one of s3, gcs or bundle must be set"
type KnowledgeBaseSource struct {
	// +optional
	S3 *S3Source `json:"s3,omitempty"`
	// +optional
	GCS *GCSSource `json:"gcs,omitempty"`
	// +optional
	Bundle *BundleSource `json:"bundle,omitempty"`
}

// S3Source is a prefix in an S3-compatible bucket. Credentials come
// from the pod's environment, never from this resource.
// +kubebuilder:validation:XValidation:rule="!has(self.endpoint) || !self.endpoint.startsWith('http://') || (has(self.insecure) && self.insecure)",message="a plaintext http:// endpoint needs insecure: true"
type S3Source struct {
	// Bucket is a bucket name, never a path: providers differ on the
	// rest (Garage accepts names AWS refuses), so only a slash is
	// refused here, as the content source does.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^/]+$`
	Bucket string `json:"bucket"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Prefix string `json:"prefix,omitempty"`
	// Endpoint is the store's URL; empty for AWS.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	Region string `json:"region,omitempty"`
	// +optional
	PathStyle bool `json:"pathStyle,omitempty"`
	// Insecure accepts a plaintext http:// Endpoint, as in a type: s3
	// content source.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// GCSSource is a prefix in a Google Cloud Storage bucket, read with the
// pod's application default credentials.
type GCSSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=222
	// +kubebuilder:validation:Pattern=`^[^/]+$`
	Bucket string `json:"bucket"`
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Prefix string `json:"prefix,omitempty"`
}

// BundleSource is a released knowledge-base bundle fetched over https
// and verified against its digest.
type BundleSource struct {
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`
	// SHA256 is the bundle's hex digest.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	SHA256 string `json:"sha256"`
}

// Access is a tree manifest's access: block.
type Access struct {
	// Paths lists the tree paths (root/platform/flux) this
	// KnowledgeBase may be reached through; empty means any.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Paths []string `json:"paths,omitempty"`
	// Identities lists the principals (authz subjects) that may reach
	// it; empty means anyone the server's own rules admit.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Identities []string `json:"identities,omitempty"`
}

// KnowledgeBaseResources bound a KnowledgeBase's resident footprint.
type KnowledgeBaseResources struct {
	// MaxResidentBytes caps what this KnowledgeBase keeps in memory.
	// +optional
	MaxResidentBytes *resource.Quantity `json:"maxResidentBytes,omitempty"`
}

// Refresh is a source's hot-reload poll.
type Refresh struct {
	// Interval is how often the source is polled. The server's floor
	// is 5s.
	Interval metav1.Duration `json:"interval"`
}

// KnowledgeBaseStatus is what the operator last observed.
type KnowledgeBaseStatus struct {
	// Mounted is true while some instance serves this KnowledgeBase.
	// +optional
	Mounted bool `json:"mounted,omitempty"`
	// ObservedGeneration is the metadata.generation the operator last
	// acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ContentGeneration is the content version being served.
	// +optional
	ContentGeneration string `json:"contentGeneration,omitempty"`
	// Pages is the number of pages served.
	// +optional
	Pages int64 `json:"pages,omitempty"`
	// LastTraversal is when a retrieval last crossed into it.
	// +optional
	LastTraversal *metav1.Time `json:"lastTraversal,omitempty"`
	// Depth is its depth in the tree (root = 0).
	// +optional
	Depth int32 `json:"depth,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// KnowledgeBase is one node of a knowledge-base tree.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kb
// +kubebuilder:printcolumn:name="Parent",type=string,JSONPath=`.spec.parent`
// +kubebuilder:printcolumn:name="Depth",type=integer,JSONPath=`.status.depth`
// +kubebuilder:printcolumn:name="Mounted",type=boolean,JSONPath=`.status.mounted`
type KnowledgeBase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnowledgeBaseSpec   `json:"spec,omitempty"`
	Status KnowledgeBaseStatus `json:"status,omitempty"`
}

// KnowledgeBaseList is a list of KnowledgeBases.
// +kubebuilder:object:root=true
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}
