package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

func testMob(placement string) *v1alpha1.MeerkatMob {
	return &v1alpha1.MeerkatMob{
		ObjectMeta: metav1.ObjectMeta{Name: "mob", Namespace: ns, UID: "uid-1"},
		Spec: v1alpha1.MeerkatMobSpec{
			Root:      "root",
			Placement: placement,
			Auth:      &v1alpha1.Auth{Issuer: "https://issuer.example", Audience: "meerkat"},
		},
	}
}

// tree is root -> {platform -> flux, vendors}, with tiers 0, 1, 2, 1.
func tree() []v1alpha1.KnowledgeBase {
	mk := func(name, parent string, tier int32) v1alpha1.KnowledgeBase {
		k := *kb(name, parent)
		k.Spec.Tier = tier
		k.Spec.Source.S3.Prefix = name + "/"
		return k
	}
	// Out of order on purpose: subtree sorts.
	return []v1alpha1.KnowledgeBase{
		mk("vendors", "root", 1), mk("flux", "platform", 2), mk("root", "", 0), mk("platform", "root", 1),
		mk("elsewhere", "", 0),
	}
}

func names(kbs []v1alpha1.KnowledgeBase) string {
	var out []string
	for _, k := range kbs {
		out = append(out, k.Name)
	}
	return strings.Join(out, ",")
}

func TestSubtree(t *testing.T) {
	got, ok := subtree("root", tree())
	if !ok || names(got) != "root,platform,flux,vendors" {
		t.Errorf("subtree(root) = %s, %v", names(got), ok)
	}
	if got, ok := subtree("platform", tree()); !ok || names(got) != "platform,flux" {
		t.Errorf("subtree(platform) = %s, %v", names(got), ok)
	}
	if _, ok := subtree("nope", tree()); ok {
		t.Error("a missing root was found")
	}
	// A loop below the root ends instead of recursing.
	loop := []v1alpha1.KnowledgeBase{*kb("a", "b"), *kb("b", "a")}
	if got, ok := subtree("a", loop); !ok || names(got) != "a,b" {
		t.Errorf("subtree of a loop = %s, %v", names(got), ok)
	}
}

func TestPlanUnits(t *testing.T) {
	kbs, _ := subtree("root", tree())
	unitsOf := func(placement string, kbs []v1alpha1.KnowledgeBase) string {
		var out []string
		for _, u := range planUnits(testMob(placement), kbs) {
			out = append(out, u.Name+"="+names(u.KBs))
		}
		return strings.Join(out, " ")
	}
	for placement, want := range map[string]string{
		v1alpha1.PlacementSingle:  "mob-all=root,platform,flux,vendors",
		v1alpha1.PlacementPerKB:   "mob-kb-root=root mob-kb-platform=platform mob-kb-flux=flux mob-kb-vendors=vendors",
		v1alpha1.PlacementPerTier: "mob-tier-0=root mob-tier-1=platform,vendors mob-tier-2=flux",
	} {
		if got := unitsOf(placement, kbs); got != want {
			t.Errorf("%s: %s, want %s", placement, got, want)
		}
	}

	// A dedicated KnowledgeBase gets its own unit under every placement.
	kbs[1].Spec.Placement = v1alpha1.PlacementDedicated // platform
	for placement, want := range map[string]string{
		v1alpha1.PlacementSingle:  "mob-all=root,flux,vendors mob-kb-platform=platform",
		v1alpha1.PlacementPerTier: "mob-tier-0=root mob-kb-platform=platform mob-tier-2=flux mob-tier-1=vendors",
	} {
		if got := unitsOf(placement, kbs); got != want {
			t.Errorf("%s with a dedicated platform: %s, want %s", placement, got, want)
		}
	}
}

func TestObjectNameAndLabelValue(t *testing.T) {
	if got := objectName("mob-kb-platform"); got != "mob-kb-platform" {
		t.Errorf("a valid name was changed: %q", got)
	}
	long := strings.Repeat("a", 60) + "-kb-" + strings.Repeat("b", 60)
	for _, s := range []string{"mob-kb-team.platform", long, "1mob-all", "Mob-All", "-x"} {
		got := objectName(s)
		if errs := validation.IsDNS1035Label(got); len(errs) > 0 {
			t.Errorf("objectName(%q) = %q: %v", s, got, errs)
		}
		if objectName(s) != got {
			t.Errorf("objectName(%q) is not stable", s)
		}
	}
	if objectName(long) == objectName(long+"c") {
		t.Error("two long names collided")
	}
	if got := labelValue("team.platform"); got != "team.platform" {
		t.Errorf("a valid label value was changed: %q", got)
	}
	for _, s := range []string{strings.Repeat("x", 100), strings.Repeat(".", 80), "kb-" + strings.Repeat("a.", 40)} {
		got := labelValue(s)
		if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
			t.Errorf("labelValue(%q) = %q: %v", s, got, errs)
		}
	}
}

func TestRenderConfig_LoadsInTheServer(t *testing.T) {
	kbs, _ := subtree("root", tree())
	kbs[0].Spec.Source = v1alpha1.KnowledgeBaseSource{S3: &v1alpha1.S3Source{
		Bucket: "kb", Prefix: "root/", Endpoint: "http://garage:3900", Region: "garage", PathStyle: true, Insecure: true,
	}}
	kbs[0].Spec.Refresh = &v1alpha1.Refresh{Interval: metav1.Duration{Duration: 5 * time.Minute}}
	kbs[0].Spec.Description = "the root hub"
	kbs[1].Spec.Source = v1alpha1.KnowledgeBaseSource{GCS: &v1alpha1.GCSSource{Bucket: "kb", Prefix: "platform/"}}
	kbs[2].Spec.Source = v1alpha1.KnowledgeBaseSource{Bundle: &v1alpha1.BundleSource{URL: "https://kb.example/flux.tar.gz", SHA256: strings.Repeat("ab", 32)}}

	auth, err := renderAuth(testMob("").Spec.Auth, "resource: https://meerkat.example/mcp\nrules:\n  - name: team\n    groups: [team]\n    collections: ['*']\n")
	if err != nil {
		t.Fatal(err)
	}
	obs := renderTelemetry(&v1alpha1.Telemetry{Endpoint: "otel-collector:4317", Protocol: v1alpha1.ProtocolGRPC, Insecure: true})
	u := planUnits(testMob(v1alpha1.PlacementSingle), kbs)[0]
	body, err := renderConfig(&u, auth, obs)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	cfg, err := contentsource.Parse(body, "rendered")
	if err != nil {
		t.Fatalf("the server refuses the rendered file: %v\n%s", err, body)
	}
	if len(cfg.Collections) != 4 {
		t.Fatalf("collections = %d\n%s", len(cfg.Collections), body)
	}
	root, platform, flux := cfg.Collections[0], cfg.Collections[1], cfg.Collections[2]
	if root.Name != "root" || root.Type != contentsource.TypeS3 || root.Endpoint != "http://garage:3900" || !root.PathStyle || !root.Insecure ||
		root.Region != "garage" || root.Prefix != "root/" || root.Description != "the root hub" || root.Refresh == nil || root.Refresh.Interval.Duration() != 5*time.Minute {
		t.Errorf("root = %+v", root)
	}
	if platform.Type != contentsource.TypeGCS || platform.Bucket != "kb" || platform.Prefix != "platform/" {
		t.Errorf("platform = %+v", platform)
	}
	if flux.Type != contentsource.TypeURL || flux.URL != "https://kb.example/flux.tar.gz" || flux.SHA256 != strings.Repeat("ab", 32) {
		t.Errorf("flux = %+v", flux)
	}
	if cfg.Auth == nil || cfg.Auth.Resource != "https://meerkat.example/mcp" || len(cfg.Auth.Providers) != 1 ||
		cfg.Auth.Providers[0].Issuer != "https://issuer.example" || cfg.Auth.Providers[0].Audience != "meerkat" {
		t.Errorf("auth = %+v", cfg.Auth)
	}
	if cfg.Observability == nil || !cfg.Observability.Traces.Enabled || cfg.Observability.OTLP.Endpoint != "otel-collector:4317" || !cfg.Observability.OTLP.Insecure {
		t.Errorf("observability = %+v", cfg.Observability)
	}
	if strings.Contains(string(body), "content:") || strings.Contains(string(body), "layout:") {
		t.Errorf("the file carries keys the operator does not set:\n%s", body)
	}
}

func TestRenderConfig_RefusesWhatTheServerRefuses(t *testing.T) {
	auth, err := renderAuth(testMob("").Spec.Auth, "resource: https://meerkat.example/mcp\n")
	if err != nil {
		t.Fatal(err)
	}
	// A KnowledgeBase name with a dot is a valid object name, but not a
	// collection name.
	dotted := *kb("team.platform", "")
	u := unit{Name: "mob-all", KBs: []v1alpha1.KnowledgeBase{dotted}}
	if _, err := renderConfig(&u, auth, nil); err == nil {
		t.Error("a dotted collection name was rendered")
	}
	// A refresh on a bundle: the server refuses it for type: url.
	b := *kb("bundle", "")
	b.Spec.Source = v1alpha1.KnowledgeBaseSource{Bundle: &v1alpha1.BundleSource{URL: "https://kb.example/b.tar.gz", SHA256: strings.Repeat("ab", 32)}}
	b.Spec.Refresh = &v1alpha1.Refresh{Interval: metav1.Duration{Duration: time.Minute}}
	u = unit{Name: "mob-all", KBs: []v1alpha1.KnowledgeBase{b}}
	if _, err := renderConfig(&u, auth, nil); err == nil {
		t.Error("a refreshing bundle was rendered")
	}
	// Providers without a resource: the server refuses an
	// audience-unbound resource server.
	noResource, err := renderAuth(testMob("").Spec.Auth, "")
	if err != nil {
		t.Fatal(err)
	}
	u = unit{Name: "mob-all", KBs: []v1alpha1.KnowledgeBase{*kb("root", "")}}
	u.KBs[0].Spec.Source.S3.Prefix = "root/"
	if _, err := renderConfig(&u, noResource, nil); err == nil || !strings.Contains(err.Error(), "resource") {
		t.Errorf("auth without a resource: err = %v", err)
	}
}

func TestRenderAuth(t *testing.T) {
	a := &v1alpha1.Auth{Issuer: "https://issuer.example", Audience: "meerkat", ConfigMapRef: "auth"}
	cfg, err := renderAuth(a, "resource: https://m.example\nproviders:\n  - issuer: https://issuer.example\n    audience: other\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Audience != "other" {
		t.Errorf("a provider the block lists was overridden or duplicated: %+v", cfg.Providers)
	}
	cfg, err = renderAuth(a, "resource: https://m.example\nproviders:\n  - issuer: https://second.example\n")
	if err != nil || len(cfg.Providers) != 2 {
		t.Errorf("providers = %+v, %v", cfg, err)
	}
	if _, err := renderAuth(a, "unknown_key: https://m.example\n"); err == nil || !strings.Contains(err.Error(), `ConfigMap "auth"`) {
		t.Errorf("an unknown key: err = %v", err)
	}
}

func TestDeploymentFor(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	u := planUnits(mob, []v1alpha1.KnowledgeBase{*kb("root", "")})[0]
	res, err := resourcesFor(mob, &u)
	if err != nil {
		t.Fatal(err)
	}
	d := deploymentFor(mob, &u, "ghcr.io/zegit-zoo/meerkat:0.21.0", []byte("a"), res)
	other := deploymentFor(mob, &u, "ghcr.io/zegit-zoo/meerkat:0.21.0", []byte("b"), res)
	if d.Spec.Template.Annotations[annotationConfig] == other.Spec.Template.Annotations[annotationConfig] {
		t.Error("a changed content-source.yaml does not roll the pods")
	}
	if len(d.OwnerReferences) != 1 || *d.OwnerReferences[0].UID != mob.UID || !*d.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v", d.OwnerReferences)
	}
	pod := d.Spec.Template.Spec
	if *pod.AutomountServiceAccountToken {
		t.Error("the server pod mounts a service account token")
	}
	if *pod.ServiceAccountName != "mob-meerkat" {
		t.Errorf("service account = %s, want the mob's own", *pod.ServiceAccountName)
	}
	c := pod.Containers[0]
	if *c.Image != "ghcr.io/zegit-zoo/meerkat:0.21.0" {
		t.Errorf("image = %s", *c.Image)
	}
	sc := c.SecurityContext
	if !*sc.RunAsNonRoot || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context = %+v", sc)
	}
	if got := strings.Join(c.Args, " "); got != "mcp serve-http --host=0.0.0.0 --port=4005 --path=/mcp" {
		t.Errorf("args = %s", got)
	}
	for _, a := range c.Args {
		if strings.Contains(a, "insecure") {
			t.Errorf("the server runs with %s", a)
		}
	}
	sel := d.Spec.Selector.MatchLabels
	for k, v := range sel {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("the pod template does not carry selector label %s=%s", k, v)
		}
	}
	svc := serviceFor(mob, &u)
	for k, v := range svc.Spec.Selector {
		if sel[k] != v {
			t.Errorf("the Service selects %s=%s, the Deployment does not", k, v)
		}
	}
}

// env is c's environment as name=value, a Secret reference written as
// name=secret/key (optional marked with ?).
func env(c corev1ac.ContainerApplyConfiguration) map[string]string {
	out := map[string]string{}
	for _, e := range c.Env {
		switch {
		case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
			r := e.ValueFrom.SecretKeyRef
			v := "secret/" + *r.Name + "/" + *r.Key
			if r.Optional != nil && *r.Optional {
				v += "?"
			}
			out[*e.Name] = v
		case e.Value != nil:
			out[*e.Name] = *e.Value
		}
	}
	return out
}

func TestDeploymentCredentials(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	u := planUnits(mob, []v1alpha1.KnowledgeBase{*kb("root", "")})[0]
	res, err := resourcesFor(mob, &u)
	if err != nil {
		t.Fatal(err)
	}
	e := env(deploymentFor(mob, &u, "img", []byte("a"), res).Spec.Template.Spec.Containers[0])
	if _, ok := e[v1alpha1.S3AccessKeyIDKey]; ok {
		t.Errorf("S3 credentials without spec.credentials: %v", e)
	}

	mob.Spec.Credentials = &v1alpha1.Credentials{S3SecretRef: "garage-kb"}
	e = env(deploymentFor(mob, &u, "img", []byte("a"), res).Spec.Template.Spec.Containers[0])
	for k, want := range map[string]string{
		v1alpha1.S3AccessKeyIDKey:     "secret/garage-kb/AWS_ACCESS_KEY_ID",
		v1alpha1.S3SecretAccessKeyKey: "secret/garage-kb/AWS_SECRET_ACCESS_KEY",
		v1alpha1.S3SessionTokenKey:    "secret/garage-kb/AWS_SESSION_TOKEN?",
		"MEERKAT_CONTENT_SOURCE":      contentSourcePath,
	} {
		if e[k] != want {
			t.Errorf("%s = %q, want %q", k, e[k], want)
		}
	}
	// Only the named keys: envFrom would let the Secret set
	// MEERKAT_CONTENT_SOURCE or any other variable.
	if c := deploymentFor(mob, &u, "img", []byte("a"), res).Spec.Template.Spec.Containers[0]; len(c.EnvFrom) != 0 {
		t.Errorf("envFrom = %+v", c.EnvFrom)
	}
}

func TestServiceAccountFor(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	sa := serviceAccountFor(mob)
	if *sa.Name != "mob-meerkat" || *sa.AutomountServiceAccountToken || len(sa.Annotations) != 0 {
		t.Errorf("service account = %+v", sa)
	}
	if len(sa.OwnerReferences) != 1 || !*sa.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v", sa.OwnerReferences)
	}
	mob.Spec.Credentials = &v1alpha1.Credentials{ServiceAccount: &v1alpha1.ServiceAccount{
		Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/kb-read"},
	}}
	if err := checkServiceAccount(mob.Spec.Credentials); err != nil {
		t.Fatal(err)
	}
	if sa := serviceAccountFor(mob); sa.Annotations["eks.amazonaws.com/role-arn"] != "arn:aws:iam::123456789012:role/kb-read" {
		t.Errorf("annotations = %v", sa.Annotations)
	}
	mob.Spec.Credentials.ServiceAccount.Annotations = map[string]string{"not a key!": "x"}
	if err := checkServiceAccount(mob.Spec.Credentials); err == nil || !strings.Contains(err.Error(), "spec.credentials.serviceAccount.annotations") {
		t.Errorf("a bad annotation key: err = %v", err)
	}
	// A long mob name still gives a valid name.
	mob.Name = strings.Repeat("m", 63)
	if errs := validation.IsDNS1035Label(serviceAccountName(mob)); len(errs) > 0 {
		t.Errorf("service account name %q: %v", serviceAccountName(mob), errs)
	}
}

func TestResourcesFor(t *testing.T) {
	q := func(s string) *resource.Quantity { v := resource.MustParse(s); return &v }
	sized := func(name, max string) v1alpha1.KnowledgeBase {
		k := *kb(name, "")
		if max != "" {
			k.Spec.Resources = &v1alpha1.KnowledgeBaseResources{MaxResidentBytes: q(max)}
		}
		return k
	}
	mem := func(l corev1.ResourceList) string {
		m, ok := l[corev1.ResourceMemory]
		if !ok {
			return "-"
		}
		return m.String()
	}
	for _, tc := range []struct {
		name     string
		spec     *corev1.ResourceRequirements
		kbs      []v1alpha1.KnowledgeBase
		req, lim string
		gomem    string
	}{
		{"defaults", nil, []v1alpha1.KnowledgeBase{sized("a", "")}, "128Mi", "512Mi", "483183819"},
		{"defaults plus resident", nil, []v1alpha1.KnowledgeBase{sized("a", "64Mi"), sized("b", ""), sized("c", "192Mi")}, "384Mi", "768Mi", "724775724"},
		{"spec replaces the defaults", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: *q("1Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: *q("2Gi")},
		}, []v1alpha1.KnowledgeBase{sized("a", "1Gi")}, "2Gi", "3Gi", "2899102923"},
		{"limit only", &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: *q("1Gi")},
		}, []v1alpha1.KnowledgeBase{sized("a", "1Gi")}, "-", "2Gi", "1932735276"},
		{"no memory at all", &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: *q("1")},
		}, []v1alpha1.KnowledgeBase{sized("a", "100Mi")}, "100Mi", "-", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mob := testMob(v1alpha1.PlacementSingle)
			mob.Spec.Resources = tc.spec
			u := unit{Name: "mob-all", KBs: tc.kbs}
			res, err := resourcesFor(mob, &u)
			if err != nil {
				t.Fatal(err)
			}
			if mem(res.Requests) != tc.req || mem(res.Limits) != tc.lim {
				t.Errorf("memory request %s, limit %s; want %s, %s", mem(res.Requests), mem(res.Limits), tc.req, tc.lim)
			}
			if got := goMemLimit(res); got != tc.gomem {
				t.Errorf("GOMEMLIMIT = %q, want %q", got, tc.gomem)
			}
			e := env(deploymentFor(mob, &u, "img", []byte("a"), res).Spec.Template.Spec.Containers[0])
			if e["GOMEMLIMIT"] != tc.gomem {
				t.Errorf("rendered GOMEMLIMIT = %q, want %q", e["GOMEMLIMIT"], tc.gomem)
			}
		})
	}
	// The mob's spec is not changed by raising a unit's memory.
	mob := testMob(v1alpha1.PlacementSingle)
	mob.Spec.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: *q("1Gi")}}
	if _, err := resourcesFor(mob, &unit{KBs: []v1alpha1.KnowledgeBase{sized("a", "1Gi")}}); err != nil {
		t.Fatal(err)
	}
	if m := mob.Spec.Resources.Limits[corev1.ResourceMemory]; m.String() != "1Gi" {
		t.Errorf("spec.resources was changed to %s", m.String())
	}

	mob.Spec.Resources = nil
	if _, err := resourcesFor(mob, &unit{KBs: []v1alpha1.KnowledgeBase{sized("a", "-1Mi")}}); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("a negative maxResidentBytes: err = %v", err)
	}
	mob.Spec.Resources = &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}}
	if _, err := resourcesFor(mob, &unit{}); err == nil || !strings.Contains(err.Error(), "claims") {
		t.Errorf("resource claims: err = %v", err)
	}
}
