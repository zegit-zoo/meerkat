// Package refresh is meerkat's opt-in runtime reconciliation: the
// policy an operator writes in content-source.yaml (`refresh:`), and the
// controller that acts on it.
//
// # What it is for
//
// A `type: gcs` content source and a `type: gcs` memory store are both
// resolved once, when the process starts. That is right for an immutable
// deployment — pin `generation:` and the bytes can never move — and
// wrong for the two cases this package exists for:
//
//   - a publication pipeline writes a new approved generation to the
//     bucket, and a hosted meerkat should serve it without a rollout;
//   - several replicas share one GCS memory store, and a memory written
//     through replica A should become discoverable through replica B.
//
// # The shape of one cycle
//
// Every cycle is the same four steps, and the first one is the one that
// matters:
//
//  1. PROBE. Metadata only — an object's current generation, or a
//     fingerprint over a prefix listing's (name, generation) pairs. No
//     bytes are downloaded and nothing is re-indexed.
//  2. If the probe matches what is already being served, stop. This is
//     the overwhelmingly common case and it costs one metadata call.
//  3. RESOLVE, off the request path, through the same hardened,
//     generation-preconditioned code the startup path uses, into a
//     staging cache entry of its own.
//  4. SWAP one coherent snapshot in, atomically. In-flight requests
//     finish against the snapshot they started on; new requests see the
//     new one.
//
// A failure at any step leaves the last known-good snapshot serving and
// marks the target degraded. Nothing partially downloaded, partially
// parsed or partially indexed is ever visible.
//
// # Why polling
//
// Metadata polling is portable, needs no additional IAM surface, no
// Pub/Sub topic, no subscription to leak, and no inbound path into the
// process. The cost is bounded and known: one metadata call per target
// per interval.
//
// This file holds the CONFIGURATION half — the `refresh:` block, its
// validation, and the jitter schedule derived from it. It deliberately
// imports nothing from the rest of meerkat, so that both
// internal/contentsource (which owns `Source`) and internal/memory
// (which owns the memory `Spec`) can embed it without either importing
// the other. controller.go holds the runtime half.
package refresh

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Failure policies — what a deployment does when a refresh fails.
const (
	// PolicyServeLastGood keeps serving the last snapshot that resolved,
	// parsed and indexed cleanly, and reports the collection as DEGRADED
	// through /readyz's counts, the refresh metrics and the log. The
	// collection stays READY: it is still answering queries correctly,
	// with content that is merely older than the bucket's.
	//
	// It is the default because it is almost always right. A publication
	// pipeline that pushes a broken generation, or a transient 503 from
	// the storage API, should not take a fleet of otherwise-healthy
	// replicas out of rotation — serving yesterday's approved knowledge
	// base is enormously better than serving none.
	PolicyServeLastGood = "serve-last-good"

	// PolicyUnready is PolicyServeLastGood plus reporting the collection
	// as NOT READY, so /readyz answers 503 and an orchestrator drains the
	// replica or halts the rollout.
	//
	// The content served does not change: a degraded replica still answers
	// queries from the last known-good snapshot, because failing readiness
	// is not the same as refusing to serve, and a replica that is drained
	// mid-request should still finish that request correctly.
	//
	// Choose it when stale content is a correctness problem rather than an
	// inconvenience — a collection whose whole value is that it is current.
	PolicyUnready = "unready"
)

// Policies lists the accepted `failure_policy:` values.
func Policies() []string { return []string{PolicyServeLastGood, PolicyUnready} }

// MinInterval is the shortest refresh interval a configuration may ask
// for.
//
// The bound is not about meerkat's own cost — a metadata call is cheap —
// but about the bucket's. A one-second interval across a fleet of
// replicas turns a knowledge base into a sustained metadata-QPS load on
// somebody else's quota, and no publication pipeline has ever needed
// sub-five-second propagation. An operator who wants a change live NOW
// has the admin reload trigger, which is immediate and costs exactly one
// cycle.
const MinInterval = 5 * time.Second

// Spec is a `refresh:` block: how often a mutable source is re-checked,
// how much scheduling noise to spread across replicas, and what to do
// when a check or a rebuild fails.
//
//	refresh:
//	  interval: 60s
//	  jitter: 10s
//	  failure_policy: serve-last-good
//
// Absent (the default, and what every configuration written before this
// existed has) the source is resolved once at startup and never
// re-checked — the immutable-deployment behaviour, unchanged.
type Spec struct {
	// Interval is the time between probes. Required; at least MinInterval.
	Interval Duration `yaml:"interval"`

	// Jitter is a random extra delay, in [0, jitter), added to each
	// interval independently.
	//
	// It exists because replicas of a hosted service start together, and
	// synchronised replicas probe together: without jitter, N replicas
	// produce an N-wide metadata spike every interval, and — worse — all
	// re-resolve the same new generation at the same instant, so a new
	// publication costs N simultaneous downloads. Spreading the schedule
	// turns both into a smooth trickle. Optional; zero means no spread.
	Jitter Duration `yaml:"jitter,omitempty"`

	// FailurePolicy selects what a failed refresh does to readiness:
	// serve-last-good (the default) or unready. See the constants.
	FailurePolicy string `yaml:"failure_policy,omitempty"`
}

// Every returns the configured interval.
func (s *Spec) Every() time.Duration {
	if s == nil {
		return 0
	}
	return s.Interval.Duration()
}

// Policy returns the effective failure policy: the configured value, or
// PolicyServeLastGood when none was set. A nil Spec answers the same, so
// a caller never has to distinguish "no policy" from "the default one".
func (s *Spec) Policy() string {
	if s == nil || s.FailurePolicy == "" {
		return PolicyServeLastGood
	}
	return s.FailurePolicy
}

// MarksUnready reports whether a failed refresh under this policy should
// make the collection fail its readiness probe.
func (s *Spec) MarksUnready() bool { return s.Policy() == PolicyUnready }

// Delay returns the wait before the next cycle: the interval plus a
// random offset in [0, jitter).
func (s *Spec) Delay() time.Duration {
	if s == nil {
		return 0
	}
	every := s.Interval.Duration()
	window := s.Jitter.Duration()
	if window <= 0 {
		return every
	}
	// #nosec G404 -- scheduling noise, not key material (see below).
	//nolint:gosec // G404: this is scheduling noise for spreading replica
	// probes, not a secret. A predictable jitter reveals nothing an
	// observer could not learn from the configured interval itself.
	return every + time.Duration(rand.Int64N(int64(window)))
}

// Validate checks a `refresh:` block. label is the config path a message
// should name, e.g. "collections[handbook].refresh".
//
// A nil Spec is valid: not configuring refresh is how a source stays
// resolved-once, which is the behaviour every pre-existing configuration
// has and the right one for a pinned deployment.
func (s *Spec) Validate(label string) error {
	if s == nil {
		return nil
	}
	// An unrecognised policy is an ERROR, never a silently-ignored line.
	// "the deployment ignored the word I wrote" is how an operator ends
	// up believing a stale replica would have been drained when nothing
	// was ever going to drain it. Same reasoning as
	// memory.Spec.Validate's personal_visibility.
	switch s.FailurePolicy {
	case "", PolicyServeLastGood, PolicyUnready:
	default:
		return fmt.Errorf("%s.failure_policy must be one of %s, got %q",
			label, strings.Join(Policies(), "|"), s.FailurePolicy)
	}
	if s.Interval <= 0 {
		return fmt.Errorf("%s.interval is required and must be positive (e.g. 60s) — a refresh block with no interval says how to fail but never when to look", label)
	}
	if s.Interval.Duration() < MinInterval {
		return fmt.Errorf("%s.interval is %s, below the %s minimum — polling faster than that spends a bucket's metadata quota rather than meerkat's; use the admin reload trigger for an immediate refresh",
			label, s.Interval, MinInterval)
	}
	if s.Jitter < 0 {
		return fmt.Errorf("%s.jitter must not be negative, got %s", label, s.Jitter)
	}
	if s.Jitter.Duration() >= s.Interval.Duration() {
		return fmt.Errorf("%s.jitter (%s) must be smaller than %s.interval (%s) — jitter spreads an interval across replicas, it does not replace it",
			label, s.Jitter, label, s.Interval)
	}
	return nil
}

// Duration is a time.Duration written in configuration the way a human
// writes one: "60s", "5m", "1h30m".
//
// It exists because gopkg.in/yaml.v3 has no duration type of its own: a
// plain time.Duration field parses a YAML scalar as an integer count of
// NANOSECONDS, so `interval: 60` would silently mean 60ns — a
// sixty-nanosecond poll loop, configured by an operator who typed what
// looked like sixty seconds. Requiring a unit removes the ambiguity
// rather than documenting it.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the value the way it is written in configuration.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string. A bare number is REFUSED with
// a message naming the unit, rather than being interpreted as
// nanoseconds (or guessed at as seconds) — see Duration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err != nil {
		return fmt.Errorf("a duration must be written with a unit, as a string like 60s, 5m or 1h30m (got %s)", node.Tag)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return fmt.Errorf("%q is not a duration: write it with a unit, like 60s, 5m or 1h30m", text)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML emits the duration in the same unit-bearing form
// UnmarshalYAML accepts, so a config that round-trips through this type
// comes back out readable.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
