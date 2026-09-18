// Package traversal is the opt-in traversal log (meerkat-mob issue G,
// brief §5 option A): one JSON object per reported retrieval session,
// written under telemetry/paths/<yyyy-mm-dd>/ in a local directory or
// an S3-compatible bucket, with every collection name and page ID
// replaced by an HMAC-SHA256 of it.
//
// Why a separate log rather than span attributes: the OTel disclosure
// rule (docs/design/observability.md) forbids names and IDs on spans and
// metric labels, and a test enforces it. Path ANALYSIS, though, needs
// to know which collection and which page a hop landed on. Hashing
// squares the two: the log carries path identity, but only the holder
// of the key — the librarian agent — can join it back to the manifest.
// The initial query is stored in plaintext by decision (2026-09-17): it
// is what the librarian reads to judge how well the client asked and
// how well meerkat routed weak prompting.
//
// Retention is an application job (Garage has no lifecycle rules) and
// defaults to unbounded: this log is the training data for the
// self-improving loop. Every object has a unique key, so the
// single-writer-per-key rule (docs/design/object-stores.md) holds on
// every provider.
package traversal

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Backends.
const (
	BackendLocal = "local"
	BackendS3    = "s3"
)

// Prefix is where the log lives under the backend's root (a directory
// or a bucket prefix).
const Prefix = "telemetry/paths/"

// Config is the `observability.traversal_log:` block.
type Config struct {
	// Backend is local or s3.
	Backend string `yaml:"backend"`
	// Path is the local directory (backend: local).
	Path string `yaml:"path,omitempty"`
	// Bucket, Prefix, Endpoint, Region and PathStyle address the store
	// (backend: s3), with the same meaning as a type: s3 content source.
	// Prefix is prepended to Prefix ("telemetry/paths/").
	Bucket    string `yaml:"bucket,omitempty"`
	Prefix    string `yaml:"prefix,omitempty"`
	Endpoint  string `yaml:"endpoint,omitempty"`
	Region    string `yaml:"region,omitempty"`
	PathStyle bool   `yaml:"path_style,omitempty"`
	// HMACKeyEnv names the environment variable holding the hashing
	// key. The key itself never appears in configuration. Required.
	HMACKeyEnv string `yaml:"hmac_key_env"`
	// RetentionDays deletes day prefixes older than this many days; 0
	// (the default) keeps everything.
	RetentionDays int `yaml:"retention_days,omitempty"`
}

// Validate checks the block's shape. It does not read the key: that
// happens at Open, so a missing variable fails startup, not config
// parsing in a build step.
func (c *Config) Validate(label string) error {
	if c == nil {
		return nil
	}
	switch c.Backend {
	case BackendLocal:
		if c.Path == "" {
			return fmt.Errorf("%s.path is required for backend: local", label)
		}
		if c.Bucket != "" || c.Endpoint != "" {
			return fmt.Errorf("%s: bucket/endpoint apply to backend: s3, not local", label)
		}
	case BackendS3:
		if c.Bucket == "" {
			return fmt.Errorf("%s.bucket is required for backend: s3", label)
		}
		if strings.Contains(c.Bucket, "/") {
			return fmt.Errorf("%s.bucket must be a bucket name, got %q", label, c.Bucket)
		}
		if c.Endpoint != "" && !strings.HasPrefix(c.Endpoint, "https://") && !strings.HasPrefix(c.Endpoint, "http://") {
			return fmt.Errorf("%s.endpoint must be an http(s):// URL, got %q", label, c.Endpoint)
		}
		if c.Path != "" {
			return fmt.Errorf("%s: path applies to backend: local, not s3", label)
		}
	case "":
		return fmt.Errorf("%s.backend is required — %s or %s", label, BackendLocal, BackendS3)
	default:
		return fmt.Errorf("%s.backend must be %s or %s, got %q", label, BackendLocal, BackendS3, c.Backend)
	}
	if strings.TrimSpace(c.HMACKeyEnv) == "" {
		return fmt.Errorf("%s.hmac_key_env is required: the name of the environment variable holding the HMAC key (never the key itself)", label)
	}
	if c.RetentionDays < 0 {
		return fmt.Errorf("%s.retention_days must be >= 0 (0 keeps everything)", label)
	}
	return nil
}

// Entry is one logged session. Every field that names a collection or
// a page is hashed before it reaches the sink; the initial query is
// stored as written.
type Entry struct {
	Version    int       `json:"version"`
	RecordedAt time.Time `json:"recorded_at"`
	// Session is the hashed session identifier.
	Session string `json:"session"`
	Outcome string `json:"outcome"`
	// InitialQuery is the agent's first query, verbatim (decision Q1).
	InitialQuery string `json:"initial_query,omitempty"`
	// Pages are hashed qualified page IDs the agent confirmed.
	Pages []string `json:"pages,omitempty"`
	// Attempted are hashed collection names, in the order tried.
	Attempted []string `json:"attempted,omitempty"`
	// PathShape is the tree depth of each attempted collection (-1 when
	// unknown), so path analysis has shape without identity.
	PathShape []int `json:"path_shape,omitempty"`
	// TierReached is the deepest depth among Attempted, or -1.
	TierReached int `json:"tier_reached"`
	Hops        int `json:"hops"`
	// Quality is the consumer-reported quality, when given.
	Quality *Quality `json:"quality,omitempty"`
	// Fallback is what the agent did when meerkat did not have it.
	Fallback *Fallback `json:"fallback,omitempty"`
	// IntakeID is the intake object written for the fallback, if any.
	IntakeID string `json:"intake_id,omitempty"`
}

// Quality is the consumer's assessment, each in [0, 1].
type Quality struct {
	Accuracy      float64 `json:"accuracy"`
	Completeness  float64 `json:"completeness"`
	AnswerQuality float64 `json:"answer_quality"`
	Notes         string  `json:"notes,omitempty"`
}

// Fallback is the research the agent did instead.
type Fallback struct {
	Kind    string   `json:"kind"`
	Summary string   `json:"summary,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Sink stores one object per key. Keys are unique per session and
// timestamp; a sink never overwrites in place.
type Sink interface {
	Put(ctx context.Context, key string, body []byte) error
	// Days lists the day prefixes present (yyyy-mm-dd), for retention.
	Days(ctx context.Context) ([]string, error)
	// DeleteDay removes every object under one day prefix.
	DeleteDay(ctx context.Context, day string) error
}

// Log hashes and writes entries.
type Log struct {
	sink      Sink
	key       []byte
	retention int
	now       func() time.Time

	mu         sync.Mutex
	lastPruned time.Time
}

// ErrNoKey is returned by Open when the configured environment variable
// is unset or empty.
var ErrNoKey = errors.New("traversal log: HMAC key environment variable is unset")

// Open builds a Log for cfg, reading the key from the environment. A
// nil cfg yields a nil Log, and every method on a nil Log is a no-op:
// an unconfigured deployment logs nothing.
func Open(ctx context.Context, cfg *Config, newS3 func(ctx context.Context, cfg *Config) (Sink, error)) (*Log, error) {
	if cfg == nil {
		return nil, nil
	}
	if err := cfg.Validate("observability.traversal_log"); err != nil {
		return nil, err
	}
	key := os.Getenv(cfg.HMACKeyEnv)
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoKey, cfg.HMACKeyEnv)
	}
	if len(key) < 16 {
		return nil, fmt.Errorf("traversal log: the key in %s is %d bytes; use at least 16", cfg.HMACKeyEnv, len(key))
	}
	var sink Sink
	switch cfg.Backend {
	case BackendLocal:
		sink = &localSink{dir: filepath.Join(cfg.Path, filepath.FromSlash(Prefix))}
	case BackendS3:
		if newS3 == nil {
			return nil, errors.New("traversal log: no s3 sink constructor")
		}
		s, err := newS3(ctx, cfg)
		if err != nil {
			return nil, err
		}
		sink = s
	}
	l := NewLog(sink, []byte(key), cfg.RetentionDays)
	if err := l.Prune(ctx); err != nil {
		return nil, fmt.Errorf("traversal log: initial retention pass: %w", err)
	}
	return l, nil
}

// NewLog wires a Log to a sink directly (tests, and Open).
func NewLog(sink Sink, key []byte, retentionDays int) *Log {
	return &Log{sink: sink, key: key, retention: retentionDays, now: time.Now}
}

// Enabled reports whether entries are written anywhere.
func (l *Log) Enabled() bool { return l != nil }

// Hash returns the hex HMAC-SHA256 of v under the log's key, or "" for
// an empty v. It is what every name and ID becomes before it is stored.
func (l *Log) Hash(v string) string {
	if l == nil || v == "" {
		return ""
	}
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))
}

// Record hashes e's identifiers and writes it. Names in e.Session,
// e.Pages and e.Attempted are given in PLAINTEXT and hashed here, so a
// caller can never accidentally store a raw name by skipping a step.
func (l *Log) Record(ctx context.Context, e Entry) (key string, err error) {
	if l == nil {
		return "", nil
	}
	now := l.now().UTC()
	e.Version = 1
	e.RecordedAt = now
	e.Session = l.Hash(e.Session)
	for i, p := range e.Pages {
		e.Pages[i] = l.Hash(p)
	}
	for i, c := range e.Attempted {
		e.Attempted[i] = l.Hash(c)
	}
	if e.Hops == 0 {
		e.Hops = len(e.Attempted)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	key = path.Join(Prefix, now.Format("2006-01-02"), fmt.Sprintf("%d-%s.json", now.UnixNano(), hex.EncodeToString(nonce[:])))
	if err := l.sink.Put(ctx, key, body); err != nil {
		return "", err
	}
	l.maybePrune(ctx, now)
	return key, nil
}

// maybePrune runs retention at most once an hour on the write path.
func (l *Log) maybePrune(ctx context.Context, now time.Time) {
	if l.retention == 0 {
		return
	}
	l.mu.Lock()
	due := now.Sub(l.lastPruned) >= time.Hour
	if due {
		l.lastPruned = now
	}
	l.mu.Unlock()
	if due {
		_ = l.Prune(ctx)
	}
}

// Prune deletes day prefixes older than the retention window. With
// retention 0 it does nothing.
func (l *Log) Prune(ctx context.Context) error {
	if l == nil || l.retention == 0 {
		return nil
	}
	cutoff := l.now().UTC().AddDate(0, 0, -l.retention).Format("2006-01-02")
	days, err := l.sink.Days(ctx)
	if err != nil {
		return err
	}
	sort.Strings(days)
	for _, d := range days {
		if d < cutoff {
			if err := l.sink.DeleteDay(ctx, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- local sink --------------------------------------------------------

type localSink struct{ dir string }

func (s *localSink) Put(_ context.Context, key string, body []byte) error {
	rel := strings.TrimPrefix(key, Prefix)
	full := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".path-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), full)
}

func (s *localSink) Days(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && isDay(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (s *localSink) DeleteDay(_ context.Context, day string) error {
	if !isDay(day) {
		return fmt.Errorf("refusing to delete %q: not a day prefix", day)
	}
	return os.RemoveAll(filepath.Join(s.dir, day))
}

func isDay(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
