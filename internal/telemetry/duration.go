package telemetry

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration written the way a human writes one:
// "5s", "60s", "1m30s".
//
// It is a second copy of internal/refresh's Duration, and deliberately
// so: this package is imported BY internal/refresh's instrumentation
// path, so importing that one back would be a cycle. The reasoning
// behind requiring a unit is identical and worth restating, because the
// failure it prevents is silent — gopkg.in/yaml.v3 decodes a bare
// `timeout: 5` into a plain time.Duration field as FIVE NANOSECONDS,
// which here would mean an export timeout that expires before the
// connection is made, on a deployment whose operator typed what looked
// like five seconds.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the value the way it is written in configuration.
func (d Duration) String() string { return time.Duration(d).String() }

// parse reads a unit-bearing duration string into d.
func (d *Duration) parse(text string) error {
	parsed, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return fmt.Errorf("%q is not a duration: write it with a unit, like 5s, 60s or 1m30s", text)
	}
	*d = Duration(parsed)
	return nil
}

// UnmarshalYAML parses a duration string, refusing a bare number.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err != nil {
		return fmt.Errorf("a duration must be written with a unit, as a string like 5s, 60s or 1m30s (got %s)", node.Tag)
	}
	return d.parse(text)
}

// MarshalYAML emits the unit-bearing form UnmarshalYAML accepts.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }
