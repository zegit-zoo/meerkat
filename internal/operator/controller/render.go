package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingv1ac "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
	"github.com/zegit-zoo/meerkat/internal/refresh"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// Labels on everything the MeerkatMob reconciler renders. The mob and
// unit labels select a unit's pods and find what to prune; the
// controller owner reference decides what is the mob's.
const (
	LabelMob       = "meerkat-mob.dev/mob"
	LabelUnit      = "meerkat-mob.dev/unit"
	labelName      = "app.kubernetes.io/name"
	labelInstance  = "app.kubernetes.io/instance"
	labelComponent = "app.kubernetes.io/component"
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "meerkat-operator"

	// annotationConfig rolls a unit's pods when its content-source.yaml
	// changes: the ConfigMap is mounted by subPath, which never updates
	// in a running pod.
	annotationConfig = "meerkat-mob.dev/content-source-sha256"
)

// The server each instance runs: `mcp serve-http`, as the chart's
// default mcp mode, on the chart's paths.
const (
	serverPort        = 4005
	serverPath        = "/mcp"
	contentSourceKey  = "content-source.yaml"
	contentSourcePath = "/home/nonroot/.config/content-source.yaml"
	cacheDir          = "/home/nonroot/.cache"
	nonRootID         = int64(65532)
)

// unit is one placement unit: one Deployment, its Service and its
// ConfigMap, serving kbs.
type unit struct {
	// Name names the Deployment, Service and ConfigMap: a DNS-1035
	// label, as a Service name must be.
	Name string
	// Key says which unit of the mob it is: "all", "tier-<n>" or
	// "kb-<name>". It is the unit label's value (shortened if needed).
	Key string
	// KBs are the KnowledgeBases it serves, in tree order.
	KBs []v1alpha1.KnowledgeBase
}

// dedicated reports whether u serves a dedicated KnowledgeBase, which
// gets a NetworkPolicy of its own. Under perKB every unit is a
// kb-<name>, so the name alone does not say.
func (u *unit) dedicated() bool {
	for i := range u.KBs {
		if u.KBs[i].Spec.Placement == v1alpha1.PlacementDedicated {
			return true
		}
	}
	return false
}

// subtree is root and every KnowledgeBase below it, depth first with
// siblings by name: the order the instances list their collections in.
// ok is false when root does not exist.
func subtree(root string, kbs []v1alpha1.KnowledgeBase) (out []v1alpha1.KnowledgeBase, ok bool) {
	byName := make(map[string]*v1alpha1.KnowledgeBase, len(kbs))
	children := map[string][]string{}
	for i := range kbs {
		k := &kbs[i]
		byName[k.Name] = k
		if k.Spec.Parent != "" && k.Spec.Parent != k.Name {
			children[k.Spec.Parent] = append(children[k.Spec.Parent], k.Name)
		}
	}
	if byName[root] == nil {
		return nil, false
	}
	for _, c := range children {
		sort.Strings(c)
	}
	seen := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return // a loop; the tree check refuses it
		}
		seen[n] = true
		out = append(out, *byName[n])
		for _, c := range children[n] {
			walk(c)
		}
	}
	walk(root)
	return out, true
}

// planUnits splits a mob's KnowledgeBases (in tree order) into
// placement units. A dedicated KnowledgeBase gets a unit of its own
// under every placement; the rest are grouped as the placement says.
// Units come out in the order their first KnowledgeBase appears.
func planUnits(mob *v1alpha1.MeerkatMob, kbs []v1alpha1.KnowledgeBase) []unit {
	var units []unit
	index := map[string]int{}
	add := func(key string, kb v1alpha1.KnowledgeBase) {
		i, ok := index[key]
		if !ok {
			i = len(units)
			index[key] = i
			units = append(units, unit{Name: objectName(mob.Name + "-" + key), Key: key})
		}
		units[i].KBs = append(units[i].KBs, kb)
	}
	for _, kb := range kbs {
		switch {
		case kb.Spec.Placement == v1alpha1.PlacementDedicated || mob.Spec.Placement == v1alpha1.PlacementPerKB:
			add("kb-"+kb.Name, kb)
		case mob.Spec.Placement == v1alpha1.PlacementPerTier:
			add("tier-"+strconv.Itoa(int(kb.Spec.Tier)), kb)
		default:
			add("all", kb)
		}
	}
	return units
}

// objectName is s when it is a DNS-1035 label (a Service name), or a
// shortened, hashed form of it that is.
func objectName(s string) string {
	if len(validation.IsDNS1035Label(s)) == 0 {
		return s
	}
	h := shortHash(s)
	b := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
	if len(b) > validation.DNS1035LabelMaxLength-len(h)-1 {
		b = b[:validation.DNS1035LabelMaxLength-len(h)-1]
	}
	b = strings.Trim(b, "-")
	if b == "" || b[0] < 'a' || b[0] > 'z' {
		b = "m" + b
		if len(b) > validation.DNS1035LabelMaxLength-len(h)-1 {
			b = b[:validation.DNS1035LabelMaxLength-len(h)-1]
		}
	}
	return b + "-" + h
}

// labelValue is s when it is a valid label value, or a shortened,
// hashed form of it that is.
func labelValue(s string) string {
	if len(validation.IsValidLabelValue(s)) == 0 {
		return s
	}
	h := shortHash(s)
	b := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, s)
	if len(b) > validation.LabelValueMaxLength-len(h)-1 {
		b = b[:validation.LabelValueMaxLength-len(h)-1]
	}
	if b = strings.Trim(b, "-_."); b == "" {
		return h
	}
	return b + "-" + h
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// mobLabels select everything a mob renders.
func mobLabels(mob *v1alpha1.MeerkatMob) map[string]string {
	return map[string]string{labelManagedBy: managedBy, LabelMob: labelValue(mob.Name)}
}

// unitSelector selects one unit's pods.
func unitSelector(mob *v1alpha1.MeerkatMob, u *unit) map[string]string {
	return map[string]string{LabelMob: labelValue(mob.Name), labelInstance: u.Name}
}

func unitLabels(mob *v1alpha1.MeerkatMob, u *unit) map[string]string {
	l := mobLabels(mob)
	l[labelName] = "meerkat"
	l[labelInstance] = u.Name
	l[labelComponent] = "server"
	l[LabelUnit] = labelValue(u.Key)
	return l
}

// serverConfig is the part of content-source.yaml the operator writes.
type serverConfig struct {
	Collections   []contentsource.Collection `yaml:"collections"`
	Auth          *authz.Config              `yaml:"auth,omitempty"`
	Observability *telemetry.Config          `yaml:"observability,omitempty"`
}

// collection is kb as a collections: entry.
func collection(kb *v1alpha1.KnowledgeBase) (contentsource.Collection, error) {
	c := contentsource.Collection{Name: kb.Name}
	src := &kb.Spec.Source
	switch {
	case src.S3 != nil:
		c.Type = contentsource.TypeS3
		c.Bucket, c.Prefix, c.Endpoint, c.Region = src.S3.Bucket, src.S3.Prefix, src.S3.Endpoint, src.S3.Region
		c.PathStyle, c.Insecure = src.S3.PathStyle, src.S3.Insecure
	case src.GCS != nil:
		c.Type = contentsource.TypeGCS
		c.Bucket, c.Prefix = src.GCS.Bucket, src.GCS.Prefix
	case src.Bundle != nil:
		c.Type = contentsource.TypeURL
		c.URL, c.SHA256 = src.Bundle.URL, src.Bundle.SHA256
	default:
		return c, fmt.Errorf("KnowledgeBase %q has no source", kb.Name)
	}
	if r := kb.Spec.Refresh; r != nil {
		c.Refresh = &refresh.Spec{Interval: refresh.Duration(r.Interval.Duration)}
	}
	c.Description = kb.Spec.Description
	return c, nil
}

// renderAuth is the auth: block every instance of mob shares: the
// ConfigMap's block (authYAML, empty when there is none) with
// spec.auth's issuer added as a provider unless it already lists it.
func renderAuth(a *v1alpha1.Auth, authYAML string) (*authz.Config, error) {
	cfg := &authz.Config{}
	if strings.TrimSpace(authYAML) != "" {
		dec := yaml.NewDecoder(strings.NewReader(authYAML))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("ConfigMap %q key %s is not an auth: block: %w", a.ConfigMapRef, v1alpha1.AuthConfigMapKey, err)
		}
	}
	for _, p := range cfg.Providers {
		if p.Issuer == a.Issuer {
			return cfg, nil
		}
	}
	cfg.Providers = append(cfg.Providers, authz.Provider{Issuer: a.Issuer, Audience: a.Audience})
	return cfg, nil
}

// renderTelemetry is the observability: block for spec.telemetry:
// traces on, exported to the one collector. The service name stays the
// server's default: a unit's name holds a KnowledgeBase name, which the
// disclosure rule keeps out of telemetry.
func renderTelemetry(t *v1alpha1.Telemetry) *telemetry.Config {
	if t == nil {
		return nil
	}
	return &telemetry.Config{
		Traces: telemetry.TraceConfig{Enabled: true},
		OTLP:   telemetry.OTLPConfig{Endpoint: t.Endpoint, Protocol: t.Protocol, Insecure: t.Insecure},
	}
}

// renderConfig is u's content-source.yaml, checked the way the server
// loads it.
func renderConfig(u *unit, auth *authz.Config, obs *telemetry.Config) ([]byte, error) {
	cfg := serverConfig{Auth: auth, Observability: obs}
	for i := range u.KBs {
		c, err := collection(&u.KBs[i])
		if err != nil {
			return nil, err
		}
		cfg.Collections = append(cfg.Collections, c)
	}
	var buf bytes.Buffer
	buf.WriteString("# Rendered by meerkat-operator from MeerkatMob and KnowledgeBase objects; edits are overwritten.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encoding content-source.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding content-source.yaml: %w", err)
	}
	if _, err := contentsource.Parse(buf.Bytes(), "content-source.yaml for "+u.Name); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ownerRef makes mob the controller of what it renders, so deleting the
// mob deletes them.
func ownerRef(mob *v1alpha1.MeerkatMob) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(v1alpha1.GroupVersion.String()).
		WithKind("MeerkatMob").
		WithName(mob.Name).
		WithUID(mob.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

func configMapFor(mob *v1alpha1.MeerkatMob, u *unit, body []byte) *corev1ac.ConfigMapApplyConfiguration {
	return corev1ac.ConfigMap(u.Name, mob.Namespace).
		WithLabels(unitLabels(mob, u)).
		WithOwnerReferences(ownerRef(mob)).
		WithData(map[string]string{contentSourceKey: string(body)})
}

func serviceFor(mob *v1alpha1.MeerkatMob, u *unit) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(u.Name, mob.Namespace).
		WithLabels(unitLabels(mob, u)).
		WithOwnerReferences(ownerRef(mob)).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithSelector(unitSelector(mob, u)).
			WithPorts(corev1ac.ServicePort().
				WithName("http").
				WithPort(serverPort).
				WithTargetPort(intstr.FromString("http")).
				WithProtocol(corev1.ProtocolTCP)))
}

// networkPolicyFor is a dedicated unit's NetworkPolicy. It selects the
// unit's pods only. Ingress is the serving port, from any pod in the
// mob's namespace and from spec.networkPolicy.ingressFrom. Egress is
// left open unless spec.networkPolicy.egress is set: the instance dials
// object stores, OIDC issuers, the OTLP collector and perhaps a cloud
// metadata endpoint, and NetworkPolicy cannot name a host, so only the
// mob's author can list their addresses.
func networkPolicyFor(mob *v1alpha1.MeerkatMob, u *unit) (*networkingv1ac.NetworkPolicyApplyConfiguration, error) {
	np := mob.Spec.NetworkPolicy
	if np == nil {
		np = &v1alpha1.NetworkPolicy{}
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	from := []*networkingv1ac.NetworkPolicyPeerApplyConfiguration{
		// An empty pod selector without a namespace selector is every
		// pod in the policy's own namespace.
		networkingv1ac.NetworkPolicyPeer().WithPodSelector(metav1ac.LabelSelector()),
	}
	for i := range np.IngressFrom {
		p, err := peerApply(&np.IngressFrom[i])
		if err != nil {
			return nil, fmt.Errorf("spec.networkPolicy.ingressFrom[%d]: %w", i, err)
		}
		from = append(from, p)
	}
	spec := networkingv1ac.NetworkPolicySpec().
		WithPodSelector(metav1ac.LabelSelector().WithMatchLabels(unitSelector(mob, u))).
		WithPolicyTypes(networkingv1.PolicyTypeIngress).
		WithIngress(networkingv1ac.NetworkPolicyIngressRule().
			// The one server port carries the API, the probes and /metrics.
			WithPorts(networkingv1ac.NetworkPolicyPort().WithProtocol(tcp).WithPort(intstr.FromInt32(serverPort))).
			WithFrom(from...))
	if e := np.Egress; e != nil {
		port := func(p corev1.Protocol) *networkingv1ac.NetworkPolicyPortApplyConfiguration {
			return networkingv1ac.NetworkPolicyPort().WithProtocol(p).WithPort(intstr.FromInt32(53))
		}
		rules := []*networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration{
			// DNS, so the object stores, issuers and collector resolve.
			networkingv1ac.NetworkPolicyEgressRule().WithPorts(port(udp), port(tcp)),
		}
		for i := range e.Rules {
			r, err := egressRuleApply(&e.Rules[i])
			if err != nil {
				return nil, fmt.Errorf("spec.networkPolicy.egress.rules[%d]: %w", i, err)
			}
			rules = append(rules, r)
		}
		spec.WithPolicyTypes(networkingv1.PolicyTypeEgress).WithEgress(rules...)
	}
	return networkingv1ac.NetworkPolicy(u.Name, mob.Namespace).
		WithLabels(unitLabels(mob, u)).
		WithOwnerReferences(ownerRef(mob)).
		WithSpec(spec), nil
}

// peerApply and egressRuleApply turn a typed peer or rule from the mob's
// spec into its apply configuration through JSON, which the two share,
// so every field the API knows is carried without a hand-written copy.
func peerApply(p *networkingv1.NetworkPolicyPeer) (*networkingv1ac.NetworkPolicyPeerApplyConfiguration, error) {
	out := &networkingv1ac.NetworkPolicyPeerApplyConfiguration{}
	return out, viaJSON(p, out)
}

func egressRuleApply(r *networkingv1.NetworkPolicyEgressRule) (*networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration, error) {
	out := &networkingv1ac.NetworkPolicyEgressRuleApplyConfiguration{}
	return out, viaJSON(r, out)
}

func viaJSON(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// serviceAccountName names the ServiceAccount every instance of mob
// runs as.
func serviceAccountName(mob *v1alpha1.MeerkatMob) string {
	return objectName(mob.Name + "-meerkat")
}

// serviceAccountFor is mob's ServiceAccount, carrying the annotations
// that bind it to a cloud identity. It mounts no API token: meerkat
// never talks to the Kubernetes API, and a workload identity webhook
// projects its own token.
func serviceAccountFor(mob *v1alpha1.MeerkatMob) *corev1ac.ServiceAccountApplyConfiguration {
	sa := corev1ac.ServiceAccount(serviceAccountName(mob), mob.Namespace).
		WithLabels(mobLabels(mob)).
		WithOwnerReferences(ownerRef(mob)).
		WithAutomountServiceAccountToken(false)
	if c := mob.Spec.Credentials; c != nil && c.ServiceAccount != nil && len(c.ServiceAccount.Annotations) > 0 {
		sa.WithAnnotations(c.ServiceAccount.Annotations)
	}
	return sa
}

// checkServiceAccount refuses annotations the API server would refuse,
// so a bad key is a status message rather than a retry loop.
func checkServiceAccount(c *v1alpha1.Credentials) error {
	if c == nil || c.ServiceAccount == nil {
		return nil
	}
	if errs := apivalidation.ValidateAnnotations(c.ServiceAccount.Annotations, field.NewPath("spec", "credentials", "serviceAccount", "annotations")); len(errs) > 0 {
		return errs.ToAggregate()
	}
	return nil
}

// The resources an instance gets when spec.resources is empty, as the
// chart's defaults.
var (
	defaultCPURequest    = resource.MustParse("50m")
	defaultMemoryRequest = resource.MustParse("128Mi")
	defaultMemoryLimit   = resource.MustParse("512Mi")
)

// resourcesFor is u's container resources: spec.resources (or the
// defaults), with the memory request and limit each raised by the sum
// of u's KnowledgeBases' maxResidentBytes. A memory request or limit
// that spec.resources leaves out stays out (the API server defaults a
// missing request to the limit); with neither, the resident bytes
// become the request.
func resourcesFor(mob *v1alpha1.MeerkatMob, u *unit) (corev1.ResourceRequirements, error) {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: defaultCPURequest.DeepCopy(), corev1.ResourceMemory: defaultMemoryRequest.DeepCopy()},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: defaultMemoryLimit.DeepCopy()},
	}
	if r := mob.Spec.Resources; r != nil {
		if len(r.Claims) > 0 {
			return res, fmt.Errorf("spec.resources.claims is not supported: an instance needs no dynamic resources")
		}
		res = corev1.ResourceRequirements{Requests: r.Requests.DeepCopy(), Limits: r.Limits.DeepCopy()}
	}
	var resident resource.Quantity
	for i := range u.KBs {
		kb := &u.KBs[i]
		if kb.Spec.Resources == nil || kb.Spec.Resources.MaxResidentBytes == nil {
			continue
		}
		q := *kb.Spec.Resources.MaxResidentBytes
		if q.Sign() < 0 {
			return res, fmt.Errorf("KnowledgeBase %q: resources.maxResidentBytes %s is negative", kb.Name, q.String())
		}
		resident.Add(q)
	}
	if resident.IsZero() {
		return res, nil
	}
	_, hasRequest := res.Requests[corev1.ResourceMemory]
	_, hasLimit := res.Limits[corev1.ResourceMemory]
	for _, l := range []corev1.ResourceList{res.Requests, res.Limits} {
		if m, ok := l[corev1.ResourceMemory]; ok {
			m.Add(resident)
			l[corev1.ResourceMemory] = m
		}
	}
	if !hasRequest && !hasLimit {
		// Nothing to raise: the resident bytes are the request.
		if res.Requests == nil {
			res.Requests = corev1.ResourceList{}
		}
		res.Requests[corev1.ResourceMemory] = resident
	}
	return res, nil
}

// goMemLimit is GOMEMLIMIT for a container with res: 90% of its memory
// limit, so the Go collector works harder before the kernel kills the
// pod. Empty without a memory limit.
func goMemLimit(res corev1.ResourceRequirements) string {
	m, ok := res.Limits[corev1.ResourceMemory]
	if !ok || m.Value() <= 0 {
		return ""
	}
	return strconv.FormatInt(m.Value()/10*9, 10)
}

// s3CredentialEnv maps the keys of the Secret spec.credentials names to
// the AWS default chain's variables. Only those keys are read: envFrom
// would let the Secret set any variable, MEERKAT_* among them.
func s3CredentialEnv(c *v1alpha1.Credentials) []*corev1ac.EnvVarApplyConfiguration {
	if c == nil || c.S3SecretRef == "" {
		return nil
	}
	ref := func(key string, optional bool) *corev1ac.EnvVarApplyConfiguration {
		return corev1ac.EnvVar().WithName(key).WithValueFrom(corev1ac.EnvVarSource().
			WithSecretKeyRef(corev1ac.SecretKeySelector().WithName(c.S3SecretRef).WithKey(key).WithOptional(optional)))
	}
	return []*corev1ac.EnvVarApplyConfiguration{
		ref(v1alpha1.S3AccessKeyIDKey, false),
		ref(v1alpha1.S3SecretAccessKeyKey, false),
		ref(v1alpha1.S3SessionTokenKey, true),
	}
}

// deploymentFor is u's Deployment, with the chart's security context
// and probes. body is the content-source.yaml it mounts; res are the
// container's resources (resourcesFor).
func deploymentFor(mob *v1alpha1.MeerkatMob, u *unit, image string, body []byte, res corev1.ResourceRequirements) *appsv1ac.DeploymentApplyConfiguration {
	sum := sha256.Sum256(body)
	probe := func(path string, period, failures int32) *corev1ac.ProbeApplyConfiguration {
		return corev1ac.Probe().
			WithHTTPGet(corev1ac.HTTPGetAction().WithPath(path).WithPort(intstr.FromString("http"))).
			WithPeriodSeconds(period).
			WithTimeoutSeconds(2).
			WithFailureThreshold(failures)
	}
	env := []*corev1ac.EnvVarApplyConfiguration{
		corev1ac.EnvVar().WithName("XDG_CACHE_HOME").WithValue(cacheDir),
		corev1ac.EnvVar().WithName("MEERKAT_NO_UPDATE_CHECK").WithValue("1"),
		corev1ac.EnvVar().WithName("MEERKAT_CONTENT_SOURCE").WithValue(contentSourcePath),
	}
	if v := goMemLimit(res); v != "" {
		env = append(env, corev1ac.EnvVar().WithName("GOMEMLIMIT").WithValue(v))
	}
	env = append(env, s3CredentialEnv(mob.Spec.Credentials)...)
	resources := corev1ac.ResourceRequirements()
	if len(res.Requests) > 0 {
		resources.WithRequests(res.Requests)
	}
	if len(res.Limits) > 0 {
		resources.WithLimits(res.Limits)
	}
	container := corev1ac.Container().
		WithName("meerkat").
		WithImage(image).
		WithArgs("mcp", "serve-http", "--host=0.0.0.0", "--port="+strconv.Itoa(serverPort), "--path="+serverPath).
		WithPorts(corev1ac.ContainerPort().WithName("http").WithContainerPort(serverPort).WithProtocol(corev1.ProtocolTCP)).
		WithEnv(env...).
		// The listener opens only once content is resolved and indexed,
		// so startup may take up to 10 minutes, as in the chart.
		WithStartupProbe(probe("/livez", 5, 120)).
		WithLivenessProbe(probe("/livez", 10, 3)).
		WithReadinessProbe(probe("/readyz", 5, 2)).
		WithResources(resources).
		WithSecurityContext(corev1ac.SecurityContext().
			WithRunAsNonRoot(true).
			WithRunAsUser(nonRootID).
			WithRunAsGroup(nonRootID).
			WithReadOnlyRootFilesystem(true).
			WithAllowPrivilegeEscalation(false).
			WithCapabilities(corev1ac.Capabilities().WithDrop("ALL")).
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault))).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("cache").WithMountPath(cacheDir),
			corev1ac.VolumeMount().WithName("content-source").WithMountPath(contentSourcePath).WithSubPath(contentSourceKey).WithReadOnly(true),
		)
	pod := corev1ac.PodSpec().
		WithServiceAccountName(serviceAccountName(mob)).
		// meerkat never talks to the Kubernetes API.
		WithAutomountServiceAccountToken(false).
		WithSecurityContext(corev1ac.PodSecurityContext().
			WithRunAsNonRoot(true).
			WithRunAsUser(nonRootID).
			WithRunAsGroup(nonRootID).
			WithFSGroup(nonRootID).
			WithSeccompProfile(corev1ac.SeccompProfile().WithType(corev1.SeccompProfileTypeRuntimeDefault))).
		WithContainers(container).
		WithVolumes(
			corev1ac.Volume().WithName("cache").WithEmptyDir(corev1ac.EmptyDirVolumeSource().WithSizeLimit(resource.MustParse("1Gi"))),
			corev1ac.Volume().WithName("content-source").WithConfigMap(corev1ac.ConfigMapVolumeSource().
				WithName(u.Name).
				WithItems(corev1ac.KeyToPath().WithKey(contentSourceKey).WithPath(contentSourceKey))),
		)
	return appsv1ac.Deployment(u.Name, mob.Namespace).
		WithLabels(unitLabels(mob, u)).
		WithOwnerReferences(ownerRef(mob)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(unitSelector(mob, u))).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(unitLabels(mob, u)).
				WithAnnotations(map[string]string{annotationConfig: hex.EncodeToString(sum[:])}).
				WithSpec(pod)))
}
