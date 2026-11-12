package controller

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

func librarianMob() *v1alpha1.MeerkatMob {
	mob := testMob(v1alpha1.PlacementPerKB)
	mob.Spec.Librarian = &v1alpha1.Librarian{Schedule: "0 3 * * *"}
	mob.Spec.Credentials = &v1alpha1.Credentials{S3SecretRef: "kb-s3"}
	return mob
}

func TestLibrarianName(t *testing.T) {
	mob := librarianMob()
	if got := librarianName(mob); got != "mob-librarian" {
		t.Errorf("librarianName = %s", got)
	}
	kbs, _ := subtree("root", tree())
	for _, u := range planUnits(mob, kbs) {
		if u.Name == librarianName(mob) {
			t.Errorf("unit %s takes the librarian's name", u.Name)
		}
	}
}

// The librarian reads the whole tree, dedicated KnowledgeBases too, and
// its config carries no auth: block, since it serves nothing.
func TestLibrarianConfig_WholeTreeWithoutAuth(t *testing.T) {
	mob := librarianMob()
	kbs, _ := subtree("root", tree())
	kbs[1].Spec.Placement = v1alpha1.PlacementDedicated
	lu := librarianUnit(mob, kbs)
	body, err := renderConfig(&lu, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := contentsource.Parse(body, "librarian")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cfg.Collections {
		got = append(got, c.Name)
	}
	if strings.Join(got, ",") != "root,platform,flux,vendors" {
		t.Errorf("collections = %v", got)
	}
	if strings.Contains(string(body), "auth:") {
		t.Errorf("the librarian's config has an auth: block:\n%s", body)
	}
}

func TestCronJobFor(t *testing.T) {
	mob := librarianMob()
	kbs, _ := subtree("root", tree())
	q := resource.MustParse("64Mi")
	kbs[0].Spec.Resources = &v1alpha1.KnowledgeBaseResources{MaxResidentBytes: &q}
	lu := librarianUnit(mob, kbs)
	res, err := resourcesFor(mob, &lu)
	if err != nil {
		t.Fatal(err)
	}
	cj := cronJobFor(mob, "ghcr.io/zegit-zoo/meerkat:0.24.0", []byte("a"), res)
	other := cronJobFor(mob, "ghcr.io/zegit-zoo/meerkat:0.24.0", []byte("b"), res)

	if *cj.Name != "mob-librarian" || *cj.Namespace != mob.Namespace {
		t.Errorf("name = %s/%s", *cj.Namespace, *cj.Name)
	}
	if len(cj.OwnerReferences) != 1 || *cj.OwnerReferences[0].UID != mob.UID || !*cj.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v", cj.OwnerReferences)
	}
	if cj.Labels[labelComponent] != librarianComponent || cj.Labels[LabelMob] != "mob" || cj.Labels[labelManagedBy] != managedBy {
		t.Errorf("labels = %v", cj.Labels)
	}
	if cj.Labels[LabelUnit] != "" {
		t.Errorf("the librarian carries a unit label %q", cj.Labels[LabelUnit])
	}
	s := cj.Spec
	if *s.Schedule != "0 3 * * *" || *s.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("schedule %q, concurrency %s", *s.Schedule, *s.ConcurrencyPolicy)
	}
	if *s.SuccessfulJobsHistoryLimit != librarianHistory || *s.FailedJobsHistoryLimit != librarianHistory {
		t.Errorf("history limits = %d/%d", *s.SuccessfulJobsHistoryLimit, *s.FailedJobsHistoryLimit)
	}
	job := s.JobTemplate.Spec
	if *job.BackoffLimit != 0 || *job.ActiveDeadlineSeconds != librarianDeadline {
		t.Errorf("backoff %d, deadline %d", *job.BackoffLimit, *job.ActiveDeadlineSeconds)
	}
	tmpl := job.Template
	if tmpl.Annotations[annotationConfig] == other.Spec.JobTemplate.Spec.Template.Annotations[annotationConfig] {
		t.Error("a changed content-source.yaml does not change the pod template")
	}
	pod := tmpl.Spec
	if *pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %s", *pod.RestartPolicy)
	}
	if *pod.AutomountServiceAccountToken || *pod.ServiceAccountName != "mob-meerkat" {
		t.Errorf("service account %s, automount %v", *pod.ServiceAccountName, *pod.AutomountServiceAccountToken)
	}
	if *pod.Volumes[1].ConfigMap.Name != "mob-librarian" {
		t.Errorf("mounts ConfigMap %s, want the librarian's", *pod.Volumes[1].ConfigMap.Name)
	}
	c := pod.Containers[0]
	// Report only: a scheduled run never files, archives or rewrites.
	if got := strings.Join(c.Args, " "); got != "ingest --role=librarian" {
		t.Errorf("args = %s", got)
	}
	if len(c.Ports) != 0 || c.ReadinessProbe != nil {
		t.Error("the librarian run exposes a port or a probe")
	}
	sc := c.SecurityContext
	if !*sc.RunAsNonRoot || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("security context = %+v", sc)
	}
	e := env(c)
	if e["AWS_ACCESS_KEY_ID"] != "secret/kb-s3/AWS_ACCESS_KEY_ID" || e["MEERKAT_CONTENT_SOURCE"] != contentSourcePath || e["MEERKAT_NO_UPDATE_CHECK"] != "1" {
		t.Errorf("env = %v", e)
	}
	// The whole tree's resident bytes are on the run's memory.
	want := defaultMemoryLimit.DeepCopy()
	want.Add(q)
	if got := (*c.Resources.Limits)[corev1.ResourceMemory]; got.Cmp(want) != 0 {
		t.Errorf("memory limit = %s, want %s", got.String(), want.String())
	}
}
