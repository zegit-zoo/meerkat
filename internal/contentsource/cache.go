package contentsource

import (
	"fmt"
	"strings"
	"time"
)

// cache.go is the `cache:` block of content-source.yaml (meerkat-mob
// issue E): the budget under which lazily mounted knowledge bases stay
// resident, and how a cold one is answered.
//
// The model is the neural-plasticity one from the brief: paths
// traversed often stay resident, rarely traversed KBs live only in
// their store and are mounted on demand, and the process stays under a
// ceiling by culling the least-travelled, oldest-first-traversed
// subtrees from the CACHE — never from content. Content lifecycle is
// the librarian's (issue H).

// Cold policies: what a request that names an unmounted lazy child
// gets.
const (
	// ColdBlocking mounts the child inside the request and answers.
	ColdBlocking = "blocking"
	// ColdAsync starts the mount and answers with a structured cold
	// status the caller retries after retry_after.
	ColdAsync = "async"
)

// CacheSpec is the `cache:` block.
type CacheSpec struct {
	// MaxBytes is the resident budget for lazily mounted collections
	// (index estimate plus page bytes per snapshot). 0 means unbounded.
	MaxBytes ByteSize `yaml:"max_bytes,omitempty"`
	// HighWatermark is the fill ratio at which culling starts; default
	// 0.65. Culling brings the cache back below it.
	HighWatermark float64 `yaml:"high_watermark,omitempty"`
	// ColdPolicy is blocking (default) or async.
	ColdPolicy string `yaml:"cold_policy,omitempty"`
	// RetryAfter is what an async cold answer tells the caller to wait;
	// default 2s.
	RetryAfter time.Duration `yaml:"retry_after,omitempty"`
	// WarmStartDays is how many days of traversal-log temperature
	// records to read at startup to pre-mount the most travelled
	// collections; 0 disables warm start. Needs the traversal log.
	WarmStartDays int `yaml:"warm_start_days,omitempty"`
	// FlushInterval is how often temperatures are written to the
	// traversal log; default 5m; needs the traversal log.
	FlushInterval time.Duration `yaml:"flush_interval,omitempty"`
}

// Defaults.
const (
	DefaultHighWatermark = 0.65
	DefaultRetryAfter    = 2 * time.Second
	DefaultFlushInterval = 5 * time.Minute
)

// Validate checks the block and fills defaults.
func (c *CacheSpec) Validate(label string) error {
	if c == nil {
		return nil
	}
	if c.MaxBytes < 0 {
		return fmt.Errorf("%s.max_bytes must be >= 0", label)
	}
	switch {
	case c.HighWatermark == 0:
		c.HighWatermark = DefaultHighWatermark
	case c.HighWatermark <= 0 || c.HighWatermark > 1:
		return fmt.Errorf("%s.high_watermark must be in (0, 1], got %v", label, c.HighWatermark)
	}
	switch c.ColdPolicy {
	case "":
		c.ColdPolicy = ColdBlocking
	case ColdBlocking, ColdAsync:
	default:
		return fmt.Errorf("%s.cold_policy must be %s or %s, got %q", label, ColdBlocking, ColdAsync, c.ColdPolicy)
	}
	if c.RetryAfter < 0 {
		return fmt.Errorf("%s.retry_after must be >= 0", label)
	}
	if c.RetryAfter == 0 {
		c.RetryAfter = DefaultRetryAfter
	}
	if c.WarmStartDays < 0 {
		return fmt.Errorf("%s.warm_start_days must be >= 0", label)
	}
	if c.FlushInterval < 0 {
		return fmt.Errorf("%s.flush_interval must be >= 0", label)
	}
	if c.FlushInterval == 0 {
		c.FlushInterval = DefaultFlushInterval
	}
	return nil
}

// ByteSize is a byte count that YAML may write as a number or as
// "64MiB" / "200MB" / "1G".
type ByteSize int64

// UnmarshalYAML accepts an integer or a size string.
func (b *ByteSize) UnmarshalYAML(unmarshal func(any) error) error {
	var n int64
	if err := unmarshal(&n); err == nil {
		*b = ByteSize(n)
		return nil
	}
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// ParseByteSize parses "64MiB", "200MB", "1G", "512k", "1024".
func ParseByteSize(s string) (ByteSize, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30},
		{"kb", 1000}, {"mb", 1000 * 1000}, {"gb", 1000 * 1000 * 1000},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"b", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(t, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			break
		}
	}
	var n float64
	if _, err := fmt.Sscanf(t, "%g", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("size %q: want a number with an optional unit (KiB, MiB, GiB, KB, MB, GB)", s)
	}
	return ByteSize(n * float64(mult)), nil
}

// String renders a byte size in the largest binary unit that divides
// it evenly, for messages and listings.
func (b ByteSize) String() string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", b>>30)
	case b >= 1<<20 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", b>>20)
	case b >= 1<<10 && b%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", b>>10)
	}
	return fmt.Sprintf("%d", int64(b))
}
