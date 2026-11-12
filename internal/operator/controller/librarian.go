package controller

import (
	"crypto/sha256"
	"encoding/hex"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

// The scheduled librarian run (spec.librarian). Each run is a Job whose
// pod loads the mob's whole tree, as `mk ingest --role librarian` does
// on a workstation, and writes the report to its log. It is that
// temporary full-tree instance: nothing else is started, and nothing
// is served.
const (
	// librarianComponent is the component label of the librarian's
	// ConfigMap, CronJob and pods.
	librarianComponent = "librarian"

	// librarianDeadline bounds one run, so a run that hangs on an
	// object store does not hold off every later one (concurrency is
	// Forbid).
	librarianDeadline = int64(2 * 60 * 60)

	// librarianHistory is how many finished Jobs of each outcome stay
	// for their logs.
	librarianHistory = int32(3)
)

// librarianName names the ConfigMap and the CronJob of mob's librarian.
// Unit names end in -all, -tier-<n> or -kb-<name>, so it never takes a
// unit's.
func librarianName(mob *v1alpha1.MeerkatMob) string {
	return objectName(mob.Name + "-librarian")
}

func librarianLabels(mob *v1alpha1.MeerkatMob) map[string]string {
	l := mobLabels(mob)
	l[labelName] = "meerkat"
	l[labelInstance] = librarianName(mob)
	l[labelComponent] = librarianComponent
	return l
}

// librarianUnit is the whole tree as one unit: the librarian reads every
// KnowledgeBase, dedicated ones included, in tree order.
func librarianUnit(mob *v1alpha1.MeerkatMob, kbs []v1alpha1.KnowledgeBase) unit {
	return unit{Name: librarianName(mob), Key: librarianComponent, KBs: kbs}
}

func librarianConfigMapFor(mob *v1alpha1.MeerkatMob, body []byte) *corev1ac.ConfigMapApplyConfiguration {
	return corev1ac.ConfigMap(librarianName(mob), mob.Namespace).
		WithLabels(librarianLabels(mob)).
		WithOwnerReferences(ownerRef(mob)).
		WithData(map[string]string{contentSourceKey: string(body)})
}

// cronJobFor is the CronJob that runs the librarian on
// spec.librarian.schedule. A run reports and changes nothing: no
// --apply, no --execute. The pod is an instance's (image, ServiceAccount,
// credentials, security context), with res from the whole tree, and it
// mounts the librarian's ConfigMap. body is that ConfigMap's
// content-source.yaml, whose hash on the pod template makes a changed
// tree visible on the CronJob.
func cronJobFor(mob *v1alpha1.MeerkatMob, image string, body []byte, res corev1.ResourceRequirements) *batchv1ac.CronJobApplyConfiguration {
	sum := sha256.Sum256(body)
	name := librarianName(mob)
	container := meerkatContainer(mob, image, res, "ingest", "--role=librarian")
	pod := meerkatPod(mob, name, container).WithRestartPolicy(corev1.RestartPolicyNever)
	return batchv1ac.CronJob(name, mob.Namespace).
		WithLabels(librarianLabels(mob)).
		WithOwnerReferences(ownerRef(mob)).
		WithSpec(batchv1ac.CronJobSpec().
			WithSchedule(mob.Spec.Librarian.Schedule).
			WithConcurrencyPolicy(batchv1.ForbidConcurrent).
			WithSuccessfulJobsHistoryLimit(librarianHistory).
			WithFailedJobsHistoryLimit(librarianHistory).
			WithJobTemplate(batchv1ac.JobTemplateSpec().
				WithLabels(librarianLabels(mob)).
				WithSpec(batchv1ac.JobSpec().
					WithBackoffLimit(0).
					WithActiveDeadlineSeconds(librarianDeadline).
					WithTemplate(corev1ac.PodTemplateSpec().
						WithLabels(librarianLabels(mob)).
						WithAnnotations(map[string]string{annotationConfig: hex.EncodeToString(sum[:])}).
						WithSpec(pod)))))
}
