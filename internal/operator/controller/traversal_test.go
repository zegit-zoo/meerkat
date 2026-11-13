package controller

import (
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

func testTraversalLog() *v1alpha1.TraversalLog {
	return &v1alpha1.TraversalLog{
		S3: v1alpha1.S3Source{
			Bucket: "telemetry", Prefix: "mob/", Endpoint: "https://garage.example", Region: "garage", PathStyle: true,
		},
		HMACKeySecretRef: "traversal-key",
	}
}

func TestRenderObservability_NothingWithoutEitherBlock(t *testing.T) {
	if obs := renderObservability(&testMob("").Spec); obs != nil {
		t.Errorf("observability = %+v, want nil", obs)
	}
}

func TestRenderObservability_TraversalLogAlone(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	mob.Spec.TraversalLog = testTraversalLog()
	obs := renderObservability(&mob.Spec)
	if obs == nil || obs.TraversalLog == nil {
		t.Fatalf("observability = %+v", obs)
	}
	if obs.Traces.Enabled || obs.OTLP.Endpoint != "" {
		t.Errorf("a traversal log alone turned the OTLP export on: %+v", obs)
	}
	want := traversal.Config{
		Backend: traversal.BackendS3, Bucket: "telemetry", Prefix: "mob/", Endpoint: "https://garage.example",
		Region: "garage", PathStyle: true, HMACKeyEnv: traversalKeyEnv,
	}
	got := *obs.TraversalLog
	if got.RetentionDays != nil || got.MaxEntriesPerDay != nil || got.MaxBytesPerDay != nil {
		t.Errorf("server defaults were overridden: %+v", got)
	}
	got.RetentionDays, got.MaxEntriesPerDay, got.MaxBytesPerDay = nil, nil, nil
	if got != want {
		t.Errorf("traversal_log = %+v, want %+v", got, want)
	}
}

func TestRenderObservability_KeepsTelemetryAndAnExplicitZeroRetention(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	mob.Spec.Telemetry = &v1alpha1.Telemetry{Endpoint: "otel-collector:4317", Protocol: v1alpha1.ProtocolGRPC, Insecure: true}
	mob.Spec.TraversalLog = testTraversalLog()
	zero := int32(0)
	mob.Spec.TraversalLog.RetentionDays = &zero
	mob.Spec.TraversalLog.Query = v1alpha1.TraversalQueryPlaintext
	obs := renderObservability(&mob.Spec)
	if !obs.Traces.Enabled || obs.OTLP.Endpoint != "otel-collector:4317" {
		t.Errorf("telemetry lost: %+v", obs)
	}
	tl := obs.TraversalLog
	if tl.RetentionDays == nil || *tl.RetentionDays != 0 || tl.Retention() != 0 {
		t.Errorf("retentionDays: 0 must keep everything, got %v", tl.RetentionDays)
	}
	if tl.Query != traversal.QueryPlaintext {
		t.Errorf("query = %q", tl.Query)
	}
}

// The rendered file is what the instances and the librarian load: the
// server must accept it, and it must name the key's variable, never
// carry a key.
func TestRenderConfig_TraversalLogLoadsInTheServer(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	mob.Spec.TraversalLog = testTraversalLog()
	obs := renderObservability(&mob.Spec)
	kbs, _ := subtree("root", tree())
	for _, u := range []unit{planUnits(mob, kbs)[0], librarianUnit(mob, kbs)} {
		auth, err := renderAuth(mob.Spec.Auth, "resource: https://meerkat.example/mcp\n")
		if err != nil {
			t.Fatal(err)
		}
		if u.Key == librarianComponent {
			auth = nil
		}
		body, err := renderConfig(&u, auth, obs)
		if err != nil {
			t.Fatalf("%s: renderConfig: %v", u.Name, err)
		}
		cfg, err := contentsource.Parse(body, u.Name)
		if err != nil {
			t.Fatalf("%s: the server refuses the rendered file: %v\n%s", u.Name, err, body)
		}
		tl := cfg.Observability.TraversalLog
		if tl == nil || tl.Backend != traversal.BackendS3 || tl.Bucket != "telemetry" || tl.HMACKeyEnv != traversalKeyEnv {
			t.Errorf("%s: traversal_log = %+v\n%s", u.Name, tl, body)
		}
		if strings.Contains(string(body), "traversal-key") {
			t.Errorf("%s: the Secret's name reached content-source.yaml:\n%s", u.Name, body)
		}
	}
}

func TestRenderConfig_RefusesAPlaintextTraversalLogEndpoint(t *testing.T) {
	mob := testMob(v1alpha1.PlacementSingle)
	mob.Spec.TraversalLog = testTraversalLog()
	mob.Spec.TraversalLog.S3.Endpoint = "http://garage:3900"
	kbs, _ := subtree("root", tree())
	lu := librarianUnit(mob, kbs)
	if _, err := renderConfig(&lu, nil, renderObservability(&mob.Spec)); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("plaintext endpoint without insecure: err = %v", err)
	}
	mob.Spec.TraversalLog.S3.Insecure = true
	if _, err := renderConfig(&lu, nil, renderObservability(&mob.Spec)); err != nil {
		t.Errorf("plaintext endpoint with insecure: %v", err)
	}
}

// Every instance and the librarian's pod read the key from the Secret,
// and only that key.
func TestTraversalLogKeyReachesEveryPod(t *testing.T) {
	mob := testMob(v1alpha1.PlacementPerKB)
	mob.Spec.Librarian = &v1alpha1.Librarian{Schedule: "0 3 * * *"}
	kbs, _ := subtree("root", tree())
	u := planUnits(mob, kbs)[0]
	res, err := resourcesFor(mob, &u)
	if err != nil {
		t.Fatal(err)
	}
	if e := env(deploymentFor(mob, &u, "img", []byte("a"), res).Spec.Template.Spec.Containers[0]); e[traversalKeyEnv] != "" {
		t.Errorf("a key without spec.traversalLog: %v", e)
	}

	mob.Spec.TraversalLog = testTraversalLog()
	want := "secret/traversal-key/" + v1alpha1.TraversalLogHMACKeyKey
	for name, c := range map[string]map[string]string{
		"instance":  env(deploymentFor(mob, &u, "img", []byte("a"), res).Spec.Template.Spec.Containers[0]),
		"librarian": env(cronJobFor(mob, "img", []byte("a"), res).Spec.JobTemplate.Spec.Template.Spec.Containers[0]),
	} {
		if c[traversalKeyEnv] != want {
			t.Errorf("%s: %s = %q, want %q", name, traversalKeyEnv, c[traversalKeyEnv], want)
		}
	}
}
