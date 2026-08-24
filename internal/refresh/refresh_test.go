package refresh

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// refresh_test.go covers the CONFIGURATION half: what a `refresh:` block
// accepts, what it refuses, and the schedule it produces.

func TestDuration_ParsesUnitBearingStrings(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"60s":    60 * time.Second,
		"5m":     5 * time.Minute,
		"1h30m":  90 * time.Minute,
		"  15s ": 15 * time.Second,
	} {
		var d Duration
		if err := yaml.Unmarshal([]byte("v: "+in), &struct {
			V *Duration `yaml:"v"`
		}{V: &d}); err != nil {
			t.Fatalf("unmarshal %q: %v", in, err)
		}
		if d.Duration() != want {
			t.Errorf("%q parsed as %s, want %s", in, d, want)
		}
	}
}

// TestDuration_RefusesABareNumber is the whole reason this type exists:
// a plain time.Duration field would read `interval: 60` as sixty
// NANOSECONDS, so an operator who wrote what looks like a minute would
// get a poll loop instead. Refusing is the only safe reading.
func TestDuration_RefusesABareNumber(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte("v: 60"), &struct {
		V *Duration `yaml:"v"`
	}{V: &d})
	if err == nil {
		t.Fatal("a bare number was accepted as a duration")
	}
	if !strings.Contains(err.Error(), "with a unit") {
		t.Errorf("error = %v, want it to name the missing unit", err)
	}
}

func TestDuration_RefusesGarbage(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte(`v: "soon"`), &struct {
		V *Duration `yaml:"v"`
	}{V: &d})
	if err == nil {
		t.Fatal(`"soon" was accepted as a duration`)
	}
}

func TestDuration_RoundTrips(t *testing.T) {
	out, err := yaml.Marshal(map[string]Duration{"interval": Duration(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "1m30s") {
		t.Errorf("marshalled as %q, want a unit-bearing form", out)
	}
}

func TestSpec_Validate(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{"no interval", Spec{}, "interval is required"},
		{"below the minimum", Spec{Interval: Duration(time.Second)}, "below the 5s minimum"},
		{"negative jitter", Spec{Interval: Duration(time.Minute), Jitter: Duration(-time.Second)}, "must not be negative"},
		{"jitter swallows the interval", Spec{Interval: Duration(time.Minute), Jitter: Duration(time.Minute)}, "must be smaller than"},
		{"unknown policy", Spec{Interval: Duration(time.Minute), FailurePolicy: "panic"}, "failure_policy must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate("collections[x].refresh")
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "collections[x].refresh") {
				t.Errorf("error = %v, want it to name the config path", err)
			}
		})
	}
}

func TestSpec_ValidateAccepts(t *testing.T) {
	for _, s := range []*Spec{
		nil,
		{Interval: Duration(MinInterval)},
		{Interval: Duration(time.Minute), Jitter: Duration(10 * time.Second)},
		{Interval: Duration(time.Minute), FailurePolicy: PolicyServeLastGood},
		{Interval: Duration(time.Minute), FailurePolicy: PolicyUnready},
	} {
		if err := s.Validate("x"); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", s, err)
		}
	}
}

// TestSpec_PolicyDefaultsToServeLastGood pins the failure direction: a
// configuration that says nothing keeps serving rather than draining the
// replica.
func TestSpec_PolicyDefaultsToServeLastGood(t *testing.T) {
	var nilSpec *Spec
	for _, s := range []*Spec{nilSpec, {}, {Interval: Duration(time.Minute)}} {
		if got := s.Policy(); got != PolicyServeLastGood {
			t.Errorf("Policy() = %q, want %q", got, PolicyServeLastGood)
		}
		if s.MarksUnready() {
			t.Error("the default policy must not fail readiness")
		}
	}
	unready := &Spec{Interval: Duration(time.Minute), FailurePolicy: PolicyUnready}
	if !unready.MarksUnready() {
		t.Error("failure_policy: unready must fail readiness")
	}
}

func TestSpec_DelayStaysWithinTheJitterWindow(t *testing.T) {
	s := &Spec{Interval: Duration(time.Minute), Jitter: Duration(10 * time.Second)}
	varied := false
	first := s.Delay()
	for range 200 {
		d := s.Delay()
		if d < time.Minute || d >= 70*time.Second {
			t.Fatalf("Delay() = %s, want [1m, 1m10s)", d)
		}
		if d != first {
			varied = true
		}
	}
	if !varied {
		t.Error("Delay() never varied — jitter is what stops replicas probing in lockstep")
	}

	// No jitter configured: the interval exactly, every time.
	plain := &Spec{Interval: Duration(30 * time.Second)}
	if got := plain.Delay(); got != 30*time.Second {
		t.Errorf("Delay() with no jitter = %s, want the bare interval", got)
	}
}
