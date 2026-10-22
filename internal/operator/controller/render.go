package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

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

// deploymentFor is u's Deployment, with the chart's security context
// and probes. body is the content-source.yaml it mounts.
func deploymentFor(mob *v1alpha1.MeerkatMob, u *unit, image string, body []byte) *appsv1ac.DeploymentApplyConfiguration {
	sum := sha256.Sum256(body)
	probe := func(path string, period, failures int32) *corev1ac.ProbeApplyConfiguration {
		return corev1ac.Probe().
			WithHTTPGet(corev1ac.HTTPGetAction().WithPath(path).WithPort(intstr.FromString("http"))).
			WithPeriodSeconds(period).
			WithTimeoutSeconds(2).
			WithFailureThreshold(failures)
	}
	container := corev1ac.Container().
		WithName("meerkat").
		WithImage(image).
		WithArgs("mcp", "serve-http", "--host=0.0.0.0", "--port="+strconv.Itoa(serverPort), "--path="+serverPath).
		WithPorts(corev1ac.ContainerPort().WithName("http").WithContainerPort(serverPort).WithProtocol(corev1.ProtocolTCP)).
		WithEnv(
			corev1ac.EnvVar().WithName("XDG_CACHE_HOME").WithValue(cacheDir),
			corev1ac.EnvVar().WithName("MEERKAT_NO_UPDATE_CHECK").WithValue("1"),
			corev1ac.EnvVar().WithName("MEERKAT_CONTENT_SOURCE").WithValue(contentSourcePath),
		).
		// The listener opens only once content is resolved and indexed,
		// so startup may take up to 10 minutes, as in the chart.
		WithStartupProbe(probe("/livez", 5, 120)).
		WithLivenessProbe(probe("/livez", 10, 3)).
		WithReadinessProbe(probe("/readyz", 5, 2)).
		WithResources(corev1ac.ResourceRequirements().
			WithRequests(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")}).
			WithLimits(corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")})).
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
