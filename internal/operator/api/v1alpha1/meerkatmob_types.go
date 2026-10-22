package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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

// The MeerkatMob conditions. Rendered says whether the operator could
// turn the mob into instances; Available says whether every instance it
// rendered has a ready replica.
const (
	ConditionRendered  = "Rendered"
	ConditionAvailable = "Available"

	// ReasonRendered: every instance's ConfigMap, Deployment and Service
	// was applied.
	ReasonRendered = "Rendered"
	// ReasonRootMissing: spec.root names no KnowledgeBase.
	ReasonRootMissing = "RootMissing"
	// ReasonImageRefused: spec.image is not on the operator's
	// allow-list.
	ReasonImageRefused = "ImageRefused"
	// ReasonConfigRefused: a KnowledgeBase, spec.auth, spec.credentials
	// or spec.resources cannot become an instance the server runs. The
	// message is the server's own load error, or names the missing or
	// conflicting piece.
	ReasonConfigRefused = "ConfigRefused"
	// ReasonTreeRefused (shared with KnowledgeBase's TreeValid): a
	// KnowledgeBase under the root breaks the depth or cycle rules.

	// ReasonAvailable: every instance has an available replica.
	ReasonAvailable = "Available"
	// ReasonProgressing: some instance has no available replica yet.
	ReasonProgressing = "Progressing"
	// ReasonNotRendered: the mob was not rendered (see Rendered), so
	// availability says nothing about the current spec.
	ReasonNotRendered = "NotRendered"
)

// AuthConfigMapKey is the key, in the ConfigMap spec.auth.configMapRef
// names, holding the server's auth: block.
const AuthConfigMapKey = "auth.yaml"

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
	// the operator was released with. The operator admits only images
	// from the repositories its --allowed-images flag lists.
	// +optional
	// +kubebuilder:validation:MaxLength=512
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

	// Credentials are what the instances authenticate to their object
	// stores with. They are referenced, never written here.
	// +optional
	Credentials *Credentials `json:"credentials,omitempty"`

	// Resources are every instance's container resources. Empty means
	// the operator's defaults (requests 50m CPU and 128Mi memory, a
	// 512Mi memory limit). Each instance's memory request and limit are
	// then raised by the maxResidentBytes of the KnowledgeBases it
	// serves.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// The S3 credential keys an instance reads from Credentials.S3SecretRef:
// the AWS default chain's variables. The session token is optional.
const (
	S3AccessKeyIDKey     = "AWS_ACCESS_KEY_ID"     //nolint:gosec // G101: false positive — a Secret key name, not a credential.
	S3SecretAccessKeyKey = "AWS_SECRET_ACCESS_KEY" //nolint:gosec // G101: false positive — a Secret key name, not a credential.
	S3SessionTokenKey    = "AWS_SESSION_TOKEN"     //nolint:gosec // G101: false positive — a Secret key name, not a credential.
)

// Credentials point the instances at a static S3 key or a cloud
// workload identity, or both.
type Credentials struct {
	// S3SecretRef names a Secret, in the mob's namespace, holding
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (and optionally
	// AWS_SESSION_TOKEN) for the s3 sources. Only those keys reach the
	// pods. The operator never reads the Secret; a pod waits until it
	// exists. Pods read it at start, so a rotation needs a restart.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	S3SecretRef string `json:"s3SecretRef,omitempty"`

	// ServiceAccount configures the ServiceAccount the operator creates
	// for the mob's instances.
	// +optional
	ServiceAccount *ServiceAccount `json:"serviceAccount,omitempty"`
}

// ServiceAccount is the mob's own ServiceAccount, named after the mob
// (<mob>-meerkat). The instances run as it, without a mounted API
// token: meerkat never talks to the Kubernetes API.
type ServiceAccount struct {
	// Annotations bind it to a cloud identity, such as
	// eks.amazonaws.com/role-arn or iam.gke.io/gcp-service-account.
	// The cloud side's trust policy names this ServiceAccount, so a mob
	// can only assume a role that trusts it.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	Annotations map[string]string `json:"annotations,omitempty"`
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
	// ConfigMapRef names a ConfigMap, in the mob's namespace, whose
	// auth.yaml key holds the server's auth: block (resource, rules,
	// and any further providers). The operator adds Issuer and Audience
	// as a provider unless the block already lists that issuer. The
	// server requires a resource once a provider is configured, so in
	// practice the ConfigMap is required.
	// +optional
	// +kubebuilder:validation:MaxLength=253
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
	// Image is the server image the instances run.
	// +optional
	Image string `json:"image,omitempty"`
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
