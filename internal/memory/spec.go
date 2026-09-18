package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// Backend type values for a `memory:` block.
const (
	// BackendLocal stores memory documents in a directory on disk.
	BackendLocal = "local"
	// BackendGCS stores them under a Google Cloud Storage prefix.
	BackendGCS = "gcs"
	// BackendS3 stores them under a prefix in an S3-compatible bucket
	// (AWS S3, Garage, MinIO). See S3Store and docs/design/object-stores.md.
	BackendS3 = "s3"
)

// DefaultLocalPath is where a `memory: {type: local}` block with no
// path: puts its documents, relative to the collection's resolved
// content directory.
//
// It is a SIBLING of the wiki root, not a directory inside it. That is
// deliberate and load-bearing: if memories lived under wiki/, the
// staging area would live there too, and an unauthorized caller's
// pending artifact would be picked up by kb.ListFS and indexed on the
// next restart — turning "staged for review" into "published, one
// restart later". Keeping the whole store outside the served tree means
// nothing in it is readable except by way of the overlay, which loads
// live documents only.
const DefaultLocalPath = "memory"

// Personal-memory read visibility, the `personal_visibility:` key of a
// `memory:` block.
const (
	// VisibilityPrivate makes a personal memory readable ONLY by the
	// principal who owns it — the verified (issuer, subject) pair its
	// namespace was derived from. It is the default, including for every
	// configuration written before the key existed, because "personal"
	// means private to a caller and a default that meant anything else
	// would be a default that discloses.
	VisibilityPrivate = "private"
	// VisibilityCollection restores the pre-#27 behaviour: a personal
	// memory is readable by every caller who can read its collection, and
	// `personal` describes only who may WRITE it.
	//
	// It exists so an operator who deliberately relied on that can say so
	// out loud, in a word that admits what it does. Configuring it
	// alongside OIDC providers produces a startup warning: under
	// authentication meerkat knows exactly whose memory each one is, and
	// choosing to show it to everybody else is a decision worth seeing in
	// the log.
	VisibilityCollection = "collection"
)

// PersonalVisibilities lists the accepted `personal_visibility:` values.
func PersonalVisibilities() []string { return []string{VisibilityPrivate, VisibilityCollection} }

// Spec is the `memory:` block of a content-source.yaml collection: the
// writable backend mk_save_memory saves into for that collection.
//
//	memory:
//	  type: local
//	  path: memory              # relative to the collection's content dir
//
//	memory:
//	  type: gcs
//	  bucket: my-org-knowledge
//	  prefix: kb/memory/
//
//	memory:
//	  type: s3
//	  bucket: my-org-knowledge
//	  prefix: kb/memory/
//	  endpoint: https://s3.example.net   # omit for AWS
//	  region: garage
//	  path_style: true                   # Garage / MinIO; not AWS
//	  sse: AES256                        # optional; SSE-S3 on AWS
//
// Absent, the collection is read-only and mk_save_memory refuses to
// name it — which is what every pre-existing configuration gets.
type Spec struct {
	// Type selects the backend: local | gcs | s3.
	Type string `yaml:"type"`

	// Path is the directory a type: local store writes into. Relative
	// paths resolve against the collection's resolved content directory.
	// Empty means DefaultLocalPath.
	Path string `yaml:"path,omitempty"`

	// Bucket / Prefix locate a type: gcs or type: s3 store. As everywhere
	// else in meerkat's object-store support there is no credentials
	// field: authentication is Application Default Credentials (GCS) or
	// the AWS default chain (S3).
	Bucket string `yaml:"bucket,omitempty"`
	Prefix string `yaml:"prefix,omitempty"`

	// Endpoint, Region and PathStyle address a type: s3 store, with the
	// same meaning as on a type: s3 content source: Endpoint is the base
	// URL for anything that is not AWS, Region the signing region,
	// PathStyle the <endpoint>/<bucket>/ addressing Garage and MinIO
	// need. SSE requests server-side encryption on every write
	// ("AES256" for SSE-S3, "aws:kms" for SSE-KMS); Garage ignores it
	// and encrypts at rest by its own configuration instead.
	Endpoint  string `yaml:"endpoint,omitempty"`
	Region    string `yaml:"region,omitempty"`
	PathStyle bool   `yaml:"path_style,omitempty"`
	SSE       string `yaml:"sse,omitempty"`

	// SingleWriter (type: s3 only) asserts that exactly one meerkat
	// process writes this store, and makes the store enforce its
	// optimistic-locking preconditions in-process instead of proving at
	// startup that the backend enforces them. It is the switch for a
	// provider that ignores If-Match / If-None-Match on writes (Garage
	// does); with it, the store gives the local backend's guarantee and
	// no more. See S3Store.
	SingleWriter bool `yaml:"single_writer,omitempty"`

	// PersonalVisibility decides who may READ this collection's personal
	// memories: private (the default — only the principal who wrote it)
	// or collection (every reader of the collection). Empty means
	// private, so the secure answer is what a configuration that says
	// nothing gets. Read it through Visibility().
	PersonalVisibility string `yaml:"personal_visibility,omitempty"`

	// Refresh opts this store into RUNTIME RECONCILIATION: a periodic,
	// metadata-only probe of the store's object listing that reloads the
	// collection's memory overlay and rebuilds its index when another
	// writer has changed something.
	//
	// It is what makes several replicas sharing one GCS memory store
	// converge. Replica A indexes its own writes immediately (see
	// collections.Collection.SaveMemory); without this, a memory written
	// through A stays invisible to B until B restarts.
	//
	// type: gcs and type: s3 only — see Validate. A local store has no
	// other writer to converge with: it is a directory belonging to one
	// process.
	Refresh *refresh.Spec `yaml:"refresh,omitempty"`
}

// Visibility returns the effective personal-memory read policy: the
// configured value, or VisibilityPrivate when none was set.
//
// A nil Spec (a collection with no memory: block) also answers private.
// Such a collection has no memory store, but the reserved page-ID prefix
// is reserved everywhere — a content page that sits under it is treated
// the same way in every collection, rather than depending on whether an
// unrelated block happens to be present.
func (s *Spec) Visibility() string {
	if s == nil || s.PersonalVisibility == "" {
		return VisibilityPrivate
	}
	return s.PersonalVisibility
}

// Validate checks a memory: block. label is the config path a message
// should name, e.g. "collections[notes].memory".
//
// contentIsEphemeral says whether the collection's resolved content
// directory is a content-addressed CACHE (a type: url or type: gcs
// source) rather than a stable directory. A relative local memory path
// under such a source would be written into a cache entry that a later
// content change replaces — the memories would appear to vanish — so it
// is refused at load time rather than discovered as data loss.
func (s *Spec) Validate(label string, contentIsEphemeral bool) error {
	if s == nil {
		return nil
	}
	// An unrecognised visibility is a configuration ERROR, never a
	// silently-ignored line: "the deployment ignored the word I wrote" is
	// exactly how a store ends up more readable than its operator
	// believes. Same reasoning as authz.ParseCapability.
	switch s.PersonalVisibility {
	case "", VisibilityPrivate, VisibilityCollection:
	default:
		return fmt.Errorf("%s.personal_visibility must be %s or %s, got %q",
			label, VisibilityPrivate, VisibilityCollection, s.PersonalVisibility)
	}
	switch s.Type {
	case "":
		return fmt.Errorf("%s.type is required — one of %s|%s|%s", label, BackendLocal, BackendGCS, BackendS3)
	case BackendLocal:
		if s.Bucket != "" || s.Prefix != "" {
			return fmt.Errorf("%s: bucket/prefix apply to type: %s or %s, not type: %s", label, BackendGCS, BackendS3, BackendLocal)
		}
		if s.Endpoint != "" || s.Region != "" || s.PathStyle || s.SSE != "" || s.SingleWriter {
			return fmt.Errorf("%s: endpoint/region/path_style/sse/single_writer apply to type: %s, not type: %s", label, BackendS3, BackendLocal)
		}
		if contentIsEphemeral && !filepath.IsAbs(s.Path) {
			return fmt.Errorf("%s.path must be absolute for a cache-backed content source: "+
				"a relative path resolves inside the content cache, which is replaced whenever the content changes — "+
				"point it at a durable directory, or use type: %s", label, BackendGCS)
		}
	case BackendGCS:
		if s.Bucket == "" {
			return fmt.Errorf("%s.bucket is required for type: %s", label, BackendGCS)
		}
		if strings.Contains(s.Bucket, "/") {
			return fmt.Errorf("%s.bucket must be a bucket name, not a path or gs:// URL, got %q", label, s.Bucket)
		}
		if s.Prefix == "" {
			return fmt.Errorf("%s.prefix is required for type: %s — an empty prefix would name the whole bucket", label, BackendGCS)
		}
		if s.Path != "" {
			return fmt.Errorf("%s: path applies to type: %s, not type: %s", label, BackendLocal, BackendGCS)
		}
		if s.Endpoint != "" || s.Region != "" || s.PathStyle || s.SSE != "" || s.SingleWriter {
			return fmt.Errorf("%s: endpoint/region/path_style/sse/single_writer apply to type: %s, not type: %s", label, BackendS3, BackendGCS)
		}
	case BackendS3:
		if s.Bucket == "" {
			return fmt.Errorf("%s.bucket is required for type: %s", label, BackendS3)
		}
		if strings.Contains(s.Bucket, "/") {
			return fmt.Errorf("%s.bucket must be a bucket name, not a path or s3:// URL, got %q", label, s.Bucket)
		}
		if s.Prefix == "" {
			return fmt.Errorf("%s.prefix is required for type: %s — an empty prefix would name the whole bucket", label, BackendS3)
		}
		if s.Path != "" {
			return fmt.Errorf("%s: path applies to type: %s, not type: %s", label, BackendLocal, BackendS3)
		}
		if s.Endpoint != "" && !strings.HasPrefix(s.Endpoint, "https://") && !strings.HasPrefix(s.Endpoint, "http://") {
			return fmt.Errorf("%s.endpoint must be an http(s):// URL, got %q", label, s.Endpoint)
		}
		switch s.SSE {
		case "", "AES256", "aws:kms":
		default:
			return fmt.Errorf("%s.sse must be AES256 or aws:kms (or absent), got %q", label, s.SSE)
		}
	default:
		return fmt.Errorf("%s.type must be %s, %s or %s, got %q", label, BackendLocal, BackendGCS, BackendS3, s.Type)
	}
	if s.Refresh != nil && s.Type == BackendS3 && s.SingleWriter {
		return fmt.Errorf("%s: refresh: and single_writer: true contradict each other — "+
			"reconciliation exists so several writers converge, and single_writer asserts there is only this one", label)
	}
	if s.Refresh != nil && s.Type != BackendGCS && s.Type != BackendS3 {
		// A local store is a directory this process owns; there is no
		// second writer to converge with, so a refresh block on one is a
		// misunderstanding of what reconciliation is for. Refusing it is
		// better than accepting a poll loop that can only ever re-read what
		// this process itself wrote.
		return fmt.Errorf("%s.refresh applies to type: %s or %s only (this store is type: %s) — "+
			"reconciliation exists so replicas sharing one object store converge, and a local store has no other writer",
			label, BackendGCS, BackendS3, s.Type)
	}
	return s.Refresh.Validate(label + ".refresh")
}

// Open builds the store a Spec describes. contentDir is the
// collection's resolved content directory, which a relative type: local
// path resolves against; it may be empty for a collection served from
// the build-time embed, in which case a relative path is refused (there
// is no directory to be relative to).
func (s *Spec) Open(ctx context.Context, contentDir string) (Store, error) {
	if s == nil {
		return nil, nil
	}
	switch s.Type {
	case BackendLocal:
		dir := s.Path
		if dir == "" {
			dir = DefaultLocalPath
		}
		if !filepath.IsAbs(dir) {
			if contentDir == "" {
				return nil, fmt.Errorf("memory.path %q is relative but this collection has no content directory to resolve it against — give an absolute path", dir)
			}
			dir = filepath.Join(contentDir, dir)
		}
		return OpenLocal(dir)
	case BackendGCS:
		return OpenGCS(ctx, s.Bucket, s.Prefix)
	case BackendS3:
		return OpenS3(ctx, s)
	default:
		// Unreachable: Validate runs at config load time.
		return nil, fmt.Errorf("unsupported memory backend %q", s.Type)
	}
}
