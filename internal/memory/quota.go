package memory

import (
	"errors"
	"fmt"
	"strings"
)

// Quota bounds how much one writer can put in a store, so no single
// principal can grow it until it no longer loads (meerkat-mob#44).
//
// A store is partitioned by OWNER, read off the key, because the key is
// the one place the owner is recorded that the caller cannot choose
// (see Namespace):
//
//	personal/<namespace>/…             one partition per namespace
//	_staging/<scope>/<namespace>/…     one partition per proposer and scope
//	raw/<namespace>/<day>/…            one per depositor per UTC day
//	                                   (the intake store's layout)
//	team/…, global/…                   one shared partition per scope
//
// Every write that would add a document to a partition, or grow its
// bytes, past the limit is refused with a *QuotaError before anything is
// stored. A write that shrinks or keeps a partition's size is always
// admitted, so lowering a limit below what a store already holds never
// stops an owner tidying up.
//
// Keys outside those shapes (the intake store's staged/, done/ and
// parked/ markers, which the librarian writes) carry no owner and are
// not counted.
//
// The check reads the partition's listing and then writes, so two
// writers racing on one partition can overshoot it by the number of
// writes in flight. That is acceptable for its purpose: the bound is a
// ceiling against runaway growth, not an accounting system.
type Quota struct {
	// Documents and Bytes bound one namespace's partition: a personal
	// namespace, one proposer's staging area at one scope, or one
	// depositor's intake for one day.
	Documents int   `yaml:"documents,omitempty"`
	Bytes     int64 `yaml:"bytes,omitempty"`
	// SharedDocuments and SharedBytes bound each shared scope (team/ and
	// global/) as a whole. Only holders of team-write / global-write
	// write there directly; everybody else's proposals land in staging,
	// under the per-namespace limits.
	SharedDocuments int   `yaml:"shared_documents,omitempty"`
	SharedBytes     int64 `yaml:"shared_bytes,omitempty"`
}

// Quota defaults: what a `memory:` or `intake:` block that says nothing
// gets. A memory is a note, not a data dump (maxDocumentBytes is
// 256 KiB), so 200 notes and 8 MiB per person is generous; the shared
// scopes get room for a team's worth of writers. Both stay well under
// maxMemoryObjects for any one partition.
const (
	DefaultQuotaDocuments       = 200
	DefaultQuotaBytes           = 8 << 20 // 8 MiB
	DefaultQuotaSharedDocuments = 5000
	DefaultQuotaSharedBytes     = 256 << 20 // 256 MiB
)

// DefaultQuota returns the limits in force when nothing is configured.
func DefaultQuota() Quota {
	return Quota{
		Documents:       DefaultQuotaDocuments,
		Bytes:           DefaultQuotaBytes,
		SharedDocuments: DefaultQuotaSharedDocuments,
		SharedBytes:     DefaultQuotaSharedBytes,
	}
}

// effective fills every unset (zero) limit from DefaultQuota. A nil
// quota is all defaults.
func (q *Quota) effective() Quota {
	d := DefaultQuota()
	if q == nil {
		return d
	}
	out := *q
	if out.Documents == 0 {
		out.Documents = d.Documents
	}
	if out.Bytes == 0 {
		out.Bytes = d.Bytes
	}
	if out.SharedDocuments == 0 {
		out.SharedDocuments = d.SharedDocuments
	}
	if out.SharedBytes == 0 {
		out.SharedBytes = d.SharedBytes
	}
	return out
}

// validate refuses negative limits, and limits beyond what a store can
// load at all: a per-partition allowance above maxMemoryObjects is a
// promise Load could not keep.
func (q *Quota) validate(label string) error {
	if q == nil {
		return nil
	}
	if q.Documents < 0 || q.Bytes < 0 || q.SharedDocuments < 0 || q.SharedBytes < 0 {
		return fmt.Errorf("%s: limits must be positive (or absent for the default)", label)
	}
	if q.Documents > maxMemoryObjects || q.SharedDocuments > maxMemoryObjects {
		return fmt.Errorf("%s: documents and shared_documents must be at most %d, the most one store loads", label, maxMemoryObjects)
	}
	return nil
}

// ErrQuotaExceeded is returned (wrapped in a *QuotaError) by Put and
// Stage when a write would take its partition past the store's Quota.
// It is not retryable: the owner has to remove or shrink something
// first, or an operator has to raise the limit.
var ErrQuotaExceeded = errors.New("memory quota exceeded")

// QuotaError says which limit a refused write ran into. It names no
// key, namespace or location: the message reaches a remote caller.
type QuotaError struct {
	// Shared is true for the team/ and global/ partitions.
	Shared bool
	// What is "documents" or "bytes".
	What  string
	Have  int64
	Limit int64
}

func (e *QuotaError) Error() string {
	whose := "this namespace"
	if e.Shared {
		whose = "this scope"
	}
	return fmt.Sprintf("%v: %s would hold %d %s, over the limit of %d", ErrQuotaExceeded, whose, e.Have, e.What, e.Limit)
}

func (e *QuotaError) Unwrap() error { return ErrQuotaExceeded }

// rawPrefix is internal/intake's StageRaw, repeated here because intake
// imports this package and not the other way round. intake_test pins
// the two together.
const rawPrefix = "raw"

// quotaPartition returns the store-relative prefix of the partition key
// belongs to, whether it is a shared one, and false when key has no
// owner to charge.
func quotaPartition(key string) (prefix string, shared, ok bool) {
	seg := strings.Split(key, "/")
	switch {
	case seg[0] == string(ScopePersonal) && len(seg) >= 3:
		return strings.Join(seg[:2], "/") + "/", false, true
	case seg[0] == StagingPrefix && len(seg) >= 4:
		return strings.Join(seg[:3], "/") + "/", false, true
	case seg[0] == rawPrefix && len(seg) >= 4:
		return strings.Join(seg[:3], "/") + "/", false, true
	case (seg[0] == string(ScopeTeam) || seg[0] == string(ScopeGlobal)) && len(seg) >= 2:
		return seg[0] + "/", true, true
	}
	return "", false, false
}

// storedObject is one document in a partition, as a backend's listing
// reports it: its store-relative key and its size.
type storedObject struct {
	Key  string
	Size int64
}

// admit decides whether writing size bytes at key keeps key's partition
// within q, given what the partition holds now (used).
func (q Quota) admit(key string, size int, used []storedObject) error {
	_, shared, ok := quotaPartition(key)
	if !ok {
		return nil
	}
	maxDocs, maxBytes := int64(q.Documents), q.Bytes
	if shared {
		maxDocs, maxBytes = int64(q.SharedDocuments), q.SharedBytes
	}
	var count, total, existing int64
	exists := false
	for _, o := range used {
		count++
		total += o.Size
		if o.Key == key {
			exists, existing = true, o.Size
		}
	}
	newCount, newTotal := count, total-existing+int64(size)
	if !exists {
		newCount++
	}
	if newCount > count && newCount > maxDocs {
		return &QuotaError{Shared: shared, What: "documents", Have: newCount, Limit: maxDocs}
	}
	if newTotal > total && newTotal > maxBytes {
		return &QuotaError{Shared: shared, What: "bytes", Have: newTotal, Limit: maxBytes}
	}
	return nil
}

// countsTowardQuota reports whether a listed key is a document a
// partition is charged for: a .md file, not a local temp file.
func countsTowardQuota(key string) bool {
	if !strings.HasSuffix(key, ".md") {
		return false
	}
	i := strings.LastIndex(key, "/")
	return !strings.HasPrefix(key[i+1:], ".")
}
