package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func fullKB() *KnowledgeBase {
	limit := int32(2)
	q := resource.MustParse("64Mi")
	now := metav1.NewTime(time.Unix(1_800_000_000, 0))
	return &KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "kb", Labels: map[string]string{"a": "b"}},
		Spec: KnowledgeBaseSpec{
			Source: KnowledgeBaseSource{
				S3:     &S3Source{Bucket: "kb", Prefix: "platform/", Endpoint: "https://s3.example.net", PathStyle: true},
				GCS:    &GCSSource{Bucket: "g"},
				Bundle: &BundleSource{URL: "https://example.net/b.tar.gz", SHA256: "00"},
			},
			Parent: "root", Tier: 1, Mount: MountLazy, Placement: PlacementDedicated,
			Access:     &Access{Paths: []string{"root/platform"}, Identities: []string{"alice"}},
			DepthLimit: &limit,
			Resources:  &KnowledgeBaseResources{MaxResidentBytes: &q},
			Refresh:    &Refresh{Interval: metav1.Duration{Duration: time.Minute}},
		},
		Status: KnowledgeBaseStatus{
			Mounted: true, Pages: 3, Depth: 1, LastTraversal: &now,
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
}

func fullMob() *MeerkatMob {
	return &MeerkatMob{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "kb"},
		Spec: MeerkatMobSpec{
			Root: "root", Placement: PlacementPerTier,
			ScaleToZero: &ScaleToZero{Enabled: true, Idle: &metav1.Duration{Duration: 30 * time.Minute}},
			Telemetry:   &Telemetry{Endpoint: "https://collector:4318"},
			Auth:        &Auth{Issuer: "https://issuer", Audience: "meerkat"},
			Librarian:   &Librarian{Schedule: "0 3 * * *"},
		},
		Status: MeerkatMobStatus{Instances: 2, Conditions: []metav1.Condition{{Type: "Ready"}}},
	}
}

// The generated deep copies must copy, not alias: the operator mutates
// status on copies of cached objects.
func TestDeepCopy_DoesNotAlias(t *testing.T) {
	kb := fullKB()
	cp := kb.DeepCopy()
	if !reflect.DeepEqual(kb, cp) {
		t.Fatal("KnowledgeBase copy differs")
	}
	*cp.Spec.DepthLimit = 5
	cp.Spec.Access.Paths[0] = "x"
	cp.Status.Conditions[0].Type = "x"
	if *kb.Spec.DepthLimit != 2 || kb.Spec.Access.Paths[0] != "root/platform" || kb.Status.Conditions[0].Type != "Ready" {
		t.Fatal("KnowledgeBase copy aliases the original")
	}

	mob := fullMob()
	mcp := mob.DeepCopy()
	if !reflect.DeepEqual(mob, mcp) {
		t.Fatal("MeerkatMob copy differs")
	}
	mcp.Spec.ScaleToZero.Idle.Duration = 0
	if mob.Spec.ScaleToZero.Idle.Duration != 30*time.Minute {
		t.Fatal("MeerkatMob copy aliases the original")
	}

	kl := &KnowledgeBaseList{Items: []KnowledgeBase{*kb}}
	ml := &MeerkatMobList{Items: []MeerkatMob{*mob}}
	for _, o := range []runtime.Object{kb, mob, kl, ml} {
		if c := o.DeepCopyObject(); !reflect.DeepEqual(o, c) {
			t.Errorf("%T: DeepCopyObject differs", o)
		}
	}
}

func TestAddToScheme_RegistersBothKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"KnowledgeBase", "KnowledgeBaseList", "MeerkatMob", "MeerkatMobList"} {
		if !s.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize %s", kind)
		}
	}
}
