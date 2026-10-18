package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Placements of a MeerkatMob: how its KnowledgeBases are spread over
// Deployments. A dedicated KnowledgeBase gets its own Deployment under
// every placement.
const (
	PlacementPerTier = "perTier"
	PlacementPerKB   = "perKB"
	PlacementSingle  = "single"
)

// MeerkatMobSpec is a tree to serve and how to serve it.
type MeerkatMobSpec struct {
	// Root names the tree's root KnowledgeBase, in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Root string `json:"root"`

	// Placement is perTier, perKB or single (the default).
	// +optional
	// +kubebuilder:default=single
	// +kubebuilder:validation:Enum=perTier;perKB;single
	Placement string `json:"placement,omitempty"`

	// Image overrides the meerkat server image. Empty means the image
	// the operator was released with.
	// +optional
	Image string `json:"image,omitempty"`

	// ScaleToZero scales an instance whose KnowledgeBases go unvisited
	// down to no replicas.
	// +optional
	ScaleToZero *ScaleToZero `json:"scaleToZero,omitempty"`

	// Telemetry is the OTLP export every instance shares.
	// +optional
	Telemetry *Telemetry `json:"telemetry,omitempty"`

	// Auth is the hosted server's authentication, shared by every
	// instance.
	// +optional
	Auth *Auth `json:"auth,omitempty"`

	// Librarian runs the librarian role on a schedule against a
	// temporary full-tree instance.
	// +optional
	Librarian *Librarian `json:"librarian,omitempty"`
}

// ScaleToZero is the idle scale-down.
type ScaleToZero struct {
	Enabled bool `json:"enabled"`
	// Idle is how long an instance may go without a traversal before
	// it is scaled to zero. Defaults to 30m; at least 1m.
	// +optional
	// +kubebuilder:default="30m"
	Idle *metav1.Duration `json:"idle,omitempty"`
}

// OTLP protocols, as the server's observability.otlp.protocol.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// Telemetry names the OTLP collector, as the server's
// observability.otlp block.
// +kubebuilder:validation:XValidation:rule="!self.endpoint.startsWith('http://') || (has(self.insecure) && self.insecure)",message="a plaintext http:// endpoint needs insecure: true"
type Telemetry struct {
	// Endpoint is the collector: a URL, or a bare host:port. grpc drops
	// the scheme and lets insecure alone decide TLS; http/protobuf gives
	// a bare host:port https://, or http:// with insecure: true.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^(https?://[^\s]+|[^/\s]+:[0-9]{1,5})$`
	Endpoint string `json:"endpoint"`
	// Protocol is grpc (the default) or http/protobuf. An OTLP/HTTP
	// collector (usually port 4318) needs http/protobuf.
	// +optional
	// +kubebuilder:default=grpc
	// +kubebuilder:validation:Enum=grpc;http/protobuf
	Protocol string `json:"protocol,omitempty"`
	// Insecure turns TLS off, and accepts a plaintext http:// Endpoint.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
}

// Auth points the instances at an OIDC issuer. Secrets are referenced,
// never written here.
type Auth struct {
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
	// ConfigMapRef names a ConfigMap holding the server's auth: policy
	// block (authz rules), in the mob's namespace.
	// +optional
	ConfigMapRef string `json:"configMapRef,omitempty"`
}

// Librarian is the scheduled librarian run.
type Librarian struct {
	// Schedule is a five-field cron expression, as a CronJob's.
	// +kubebuilder:validation:MinLength=9
	Schedule string `json:"schedule"`
}

// MeerkatMobStatus is what the operator last observed.
type MeerkatMobStatus struct {
	// ObservedGeneration is the metadata.generation the operator last
	// acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Instances is the number of Deployments the mob renders to.
	// +optional
	Instances int32 `json:"instances,omitempty"`
	// KnowledgeBases is the number of KnowledgeBases in the tree.
	// +optional
	KnowledgeBases int32 `json:"knowledgeBases,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MeerkatMob serves a KnowledgeBase tree as one or more meerkat
// instances.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mob
// +kubebuilder:printcolumn:name="Root",type=string,JSONPath=`.spec.root`
// +kubebuilder:printcolumn:name="Placement",type=string,JSONPath=`.spec.placement`
// +kubebuilder:printcolumn:name="Instances",type=integer,JSONPath=`.status.instances`
type MeerkatMob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MeerkatMobSpec   `json:"spec,omitempty"`
	Status MeerkatMobStatus `json:"status,omitempty"`
}

// MeerkatMobList is a list of MeerkatMobs.
// +kubebuilder:object:root=true
type MeerkatMobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MeerkatMob `json:"items"`
}
