# Object stores: one interface, three providers

meerkat serves knowledge from an object store in two modes — a single
`.tar.gz` bundle, or an object prefix mounted as a directory tree —
and writes memory documents back into one with optimistic locking.
Until the S3 backend, "object store" meant Google Cloud Storage. This
page records what is shared, what differs per provider, and what each
provider was **observed** to do, so the choice of backend is a table
lookup rather than a discovery.

Order of support, as decided 2026-09-17: Garage first, then AWS S3,
then GCS parity. GCS stays a first-class backend; nothing GCS-specific
was removed.

## The seams

| Seam | Where | GCS | S3 |
|---|---|---|---|
| read client | `contentsource.objectStore` (`internal/contentsource/objectstore.go`) | `storageAPI` (`gcs.go`) | `s3ObjectStore` (`s3.go`) |
| per-provider constants | `contentsource.storeKind` | `gcsKind` | `s3Kind` |
| version token | `storedObject.Version` (opaque string) | object generation (decimal) | ETag (unquoted) |
| pin in config | | `generation:` | `etag:` |
| cheap probe | `contentsource.ObjectVersion` → `storeVersion` | `GCSVersion` | `S3Version` |
| fetch | `contentsource.FetchObject` → `fetchStore` | `FetchGCS` | `FetchS3` |
| memory store | `memory.Store` + `memory.Fingerprinter` | `GCSStore` | `S3Store` |
| client construction | | ADC, in `gcs.go` | `internal/s3api.New` |

Everything that is about *serving a bucket* — skip-if-cached, the
immutable cache key, the conditional read, the size caps, the `os.Root`
tree write, the metadata-only probe — is written once in
`objectstore.go` and parametrised by a `storeKind`. A new provider is a
new adapter plus a `storeKind`, not a new fetch path.

## What every provider must give us

1. **A version token per object** that changes whenever the bytes do,
   readable from metadata alone (`Attrs` / `HeadObject`, and every
   listing entry). GCS: the generation. S3: the ETag.
2. **A conditional read**: "give me exactly the version I name, or
   fail." GCS: `ifGenerationMatch` plus an explicit generation. S3:
   `If-Match: "<etag>"` on `GetObject`. This is what makes a cache
   entry named `<version>` provably hold that version's bytes.
3. **A complete listing** under a prefix, following pagination.
4. For a **memory store**: conditional writes. GCS: `ifGenerationMatch:
   0` to create, `ifGenerationMatch: N` to update. S3: `If-None-Match:
   *` to create, `If-Match: "<etag>"` to update. This is what lets
   several replicas share one store without losing writes.

## ETags are opaque

An S3 ETag is the MD5 of the body for a single-part unencrypted upload,
a hash of part hashes with a `-N` suffix for a multipart upload, and
something else again under SSE-KMS. meerkat **never** interprets it: it
compares it, caches under it, and sends it back in `If-Match`. If you
want a content hash verified, pin `sha256:` on a bundle — it is checked
before extraction exactly as for `type: url` and `type: gcs`.

Because an ETag is not guaranteed content-derived under every
encryption mode, the prefix-mode listing fingerprint for S3 covers
`(key, ETag, size)`. For GCS it stays `(name, generation)` — the same
bytes the pre-S3 code hashed, so an upgrade neither invalidates a GCS
prefix cache nor reports a spurious change to hot reload.

## Provider matrix

Observed by the opt-in conformance tests
(`internal/contentsource/s3_conformance_test.go`,
`internal/memory/s3_conformance_test.go`); `scripts/garage-up.sh`
starts the Garage the CI job runs them against. A row that changes
fails the test and must be updated here in the same change.

| Property | AWS S3 | Garage 2.4 | MinIO | GCS |
|---|---|---|---|---|
| addressing | virtual-host (default) | path-style (`path_style: true`) | path-style | n/a |
| `region:` | from the default chain or `region:` | must match `s3_region` (default `garage`) | any | n/a |
| ETag on `HeadObject` / listing | yes | yes | yes | generation |
| `GetObject` + `If-Match` refused with 412 | yes | **yes** (observed) | yes | yes (`ifGenerationMatch`) |
| `PutObject` + `If-None-Match: *` refused | yes | **no — silently accepted** (observed) | yes | yes |
| `PutObject` + `If-Match` refused | yes | **no — silently accepted** (observed) | yes | yes |
| shared memory store safe | yes | **no** — `single_writer: true` required | yes | yes |
| listing pagination (>1000 keys) | yes | yes (observed, 1005 keys) | yes | iterator |
| bucket lifecycle rules | yes | **no** | yes | yes |
| SSE-S3 (`sse: AES256`) | yes | ignored (encrypts at rest by its own config) | yes | n/a |
| request checksums (`x-amz-checksum-*`, aws-chunked) | required-only | required-only | required-only | n/a |

"Observed" cells were measured against `dxflrs/garage:v2.4.1` on
2026-09-18. Columns without "observed" are the provider's documented
behaviour; MinIO and AWS rows are asserted by the same tests when the
CI matrix runs them (`MEERKAT_TEST_S3_PROVIDER=minio|aws`).

### What the Garage gap means

Garage enforces the read precondition, so a `type: s3` **content
source** on Garage has every guarantee the GCS source has: the bytes
in a cache entry named `<etag>` are that ETag's bytes, and a pinned
`etag:` can never be silently replaced.

Garage does not enforce write preconditions, so an S3 **memory store**
on Garage cannot rely on the backend to refuse a stale update. Rather
than discover that in production, `OpenS3` **proves** the property at
startup: it writes a probe document under `_staging/_probe/` (never
loaded, never fingerprinted), re-writes it twice with preconditions a
conforming backend must refuse, and deletes it. Either write being
accepted returns `ErrConditionalWritesNotEnforced` and the store does
not open. The probe costs three `PUT`s and a `DELETE` per process
start and needs delete permission on the prefix — which the memory
principal is granted anyway.

The escape hatch is `single_writer: true`. The operator asserts that
exactly one meerkat process writes this store, and the store enforces
its preconditions itself: a mutex plus a `HeadObject` compare before
every `Put`. That is precisely the guarantee `type: local` gives and no
more — correct within one process, racy across two. `refresh:` and
`single_writer: true` are refused together, because reconciliation
exists so several writers converge and `single_writer` asserts there
is only this one. The conditional headers are still sent in that mode:
a Garage release that starts honouring them costs nothing.

## Retention is an application job

Garage has no bucket lifecycle rules. Nothing in meerkat may therefore
depend on lifecycle for retention — not audio, not a traversal log, not
intake staging. Retention is the librarian's job (issue H) so it
behaves the same on Garage, AWS and GCS. `_staging/` is not expired by
the store either; promotion and expiry are explicit.

## Checksums

The AWS SDK's default is to compute a CRC32 trailer with `aws-chunked`
encoding on every upload and to validate response checksums. Several
third-party stores reject or mishandle that, and meerkat already pins
content by version token and by `sha256`, so `internal/s3api.New` sets
both request and response checksum behaviour to **when required**. This
is the one SDK setting the whole codebase shares, and the reason both
packages build their client through `s3api` rather than directly.

## Credentials

The AWS default chain and nothing else: `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY`, `~/.aws/config` profiles, IRSA / web identity
on EKS, an EC2 or ECS metadata endpoint. There is no field in the
schema for a static key, mirroring the ADC-only rule for GCS. For
Garage, create a key with `garage key create` and grant it on the
bucket; hand it to the process as environment, never as config.

## Telemetry

S3 reuses the `meerkat.source.*` attributes with the bounded source
types `s3-object` / `s3-prefix`, and adds the `meerkat.s3` span with
`meerkat.s3.operation` (`attrs` | `list` | `read`) beside `meerkat.gcs`.
The memory backend label gains `s3`. The disclosure rule is unchanged
and still tested: no bucket, key, prefix, version or **endpoint** ever
reaches a span — an endpoint names a deployment's storage topology as
surely as a bucket does.

## Out of scope

Azure Blob; cross-region replication; encryption keys beyond SSE-S3 /
SSE-KMS; S3 object versioning (`VersionId`) — the ETag is enough to
name immutable bytes, and versioning is an operator choice the store
does not depend on.
