# Verification — 15 October 2026

This report describes the standalone package, not a Clara deployment or migration.
All data used for verification lived in temporary collections. No existing
personal memories were imported or edited.

## Correctness evidence

The final checks passed:

```sh
go test -race -count=1 -timeout 120s ./...
go vet ./...
CGO_ENABLED=0 go build -o memory ./cmd/memory
CGO_ENABLED=0 go build ./...
go test -coverprofile=/tmp/markdown-memory-final-coverage.out ./...
go test -run '^$' -fuzz '^FuzzParseDoesNotPanic$' -fuzztime 10s -parallel 4
./memory -help
```

There are 30 named tests plus an executable public-library example and parser
fuzz seeds. The core library has 87.0% statement coverage. CLI behavior is tested
by compiling and invoking a separate binary; those child executions are not
included in the CLI package's statement-coverage percentage.

The public example was also copied into a separate temporary Go module with a
local require/replace directive. It compiled and passed there, verifying the
documented external-consumer setup rather than only same-module imports.

| Requirement | Evidence |
|---|---|
| Verbatim Markdown body, metadata updates, large source IDs | `TestStorageAndVerbatimBody`, `TestFormatTypesAndPrecision`, `TestMetadataPatchWithoutBodyRoundTrip` |
| Authoritative full reads and safe metadata patches | `TestReadsAndPatchesUseAuthoritativeBody`; body read from the actual file even when its derived cached copy is altered |
| Arbitrary taxonomy evolution, names-only discovery, explicit entries | `TestTaxonomyEvolutionAndDiscovery`; creation/update/deletion changes names and counts, and entry-only changes leave the name fingerprint stable |
| Typed values, compound filters, ordering and pagination | `TestStructuredQueryAndSorting`, `TestQueryInputErrorsAndEmptyCollection` |
| Years/months/dates, open ranges, approximate/unknown dates | `TestTemporalSemantics`, `TestTimePrecisionAndChronologicalSort` |
| Stable entities, outgoing links, backlinks, cycles and missing targets | `TestGraph`, `TestMalformedDuplicatesAndReferences` |
| External edits, restored mtime, renames and deletion | `TestExternalEditsRenameAndNoReparse` |
| Malformed files, duplicate IDs, corrupt indexes/manifest | `TestMalformedDuplicatesAndReferences`, `TestCorruptIndexAndManifestRecover` |
| Source-only portability and reconstruction | `TestRebuildAndCopy`, `TestOriginalBodyAndCacheRebuild` |
| Concurrent writers, stale updates/deletes, cancellation | `TestConcurrentWriters`, `TestContextCancellation`, `TestLockCancellation`; four separate CLI writers also share one collection |
| Interrupted publication before index update | `TestCrashPublicationRecovery`: a real subprocess exits after the durable source publication and before index synchronization; a fresh operation recovers the record and search result |
| Isolation, traversal, symlinks, private permissions | `TestBoundaries`, `TestFilePermissions` |
| Chunking, cache invalidation, model changes, unavailable endpoint | `TestSemanticHybridCacheAndDegradation`, `TestSemanticFilteringChunksAndNoEndpoint`, `TestVectorCacheIsolationDeletionAndRebuild` |
| Invalid embedding responses and provider-error privacy | `TestEmbeddingResponseValidation` and explicit HTTP-error checks in semantic tests |
| All CLI operations, pure JSON errors, conflict exit codes | `TestCLIEndToEnd`, including metadata patching and invalid flags |

HTTP fixture tests verify mechanisms. They do not establish the linguistic quality
of an embedding model. The real-provider evaluation below is separate.

## Real-text scale measurements

Raw output: [performance.json](performance.json).

Client: Linux/amd64, Go 1.27.0, AMD Ryzen 9 9950X, 32 logical CPUs. Provider:
Clara's already-running CPU embedding service, on AMD Ryzen 5 9600X with 12 logical
CPUs (`--device none`), accessed through a temporary SSH forward. Model:
`granite-embedding-311M-multilingual-r2-Q4_K_M.gguf`, returning 768 dimensions.
No service configuration was changed.

The dataset uses Go standard-library source comment groups of
120–3,000 bytes, selected deterministically in sorted source-file order. The
available corpus contained 34,073 comment groups; these measurements did not cycle
the input list. The parser includes documentation, implementation notes, and
license headers; repeated text is not deduplicated. Each memory has a title, body
and two taxonomy values.
This is varied real text, not a representative sample of all personal memories.

Each query measurement includes collection reconciliation, manifest decoding,
index open/close and result preparation. Semantic/hybrid timings also include
cache loading, exact cosine ranking and a live query-embedding request. Filesystem
caches are warm. Five sequential samples per operation were used in this run.

| Records | Initial text index | Structured p50 | Lexical p50 | Taxonomy names p50 | Warm semantic p50 | Warm hybrid p50 |
|---:|---:|---:|---:|---:|---:|---:|
| 100 | 12.03 ms | 0.90 ms | 1.01 ms | 0.74 ms | 13.28 ms | 12.76 ms |
| 1,000 | 122.66 ms | 5.66 ms | 8.60 ms | 5.42 ms | 39.14 ms | 38.72 ms |
| 10,000 | 1,367.40 ms | 68.36 ms | 95.73 ms | 68.30 ms | 363.43 ms | 967.95 ms |

At all three sizes, an unchanged sync parsed zero records and a single external
edit parsed exactly one. For 10,000 records the edit sync took 102.30 ms. The
Markdown corpus was approximately 5.75 MB; all derived caches, including embeddings,
occupied approximately 101.6 MB.

First semantic recall, including generating the entire eligible embedding cache,
took 3.82 s / 44.38 s / 367.25 s at 100 / 1,000 / 10,000 records. This is real CPU
embedding work, not cold-start text-index latency. Plan an adequate context deadline
when warming a large collection. Subsequent calls embed changed/new content only.

Reproduction, with your own endpoint/model:

```sh
go run ./cmd/memory-measure -samples 5 -embedding-batch-size 1 \
  -embedding-base http://127.0.0.1:18390/v1 \
  -embedding-model /path/to/granite-embedding-311M-multilingual-r2-Q4_K_M.gguf
```

Port 18390 was the temporary local tunnel, not a required package port. API keys,
if needed, come from `MEMORY_EMBEDDING_API_KEY` and are not written into reports.

Two earlier load runs encountered provider-request timeouts using larger input
batches (16 and 4). Their root cause was not established; the service subsequently
answered a direct short query in 13 ms. Completed scale runs used single-input
batches. These observations do not prove an optimal or universal provider batch
size. The engine exposes configuration and explicit degraded fallback rather than
silently claiming successful semantic retrieval after a timeout.

## Real bilingual recall

The same raw report includes 18 actual searches: six deliberately hand-authored
probes in each mode. The six stored accounts include Greek originals and English
accounts, with generic titles to avoid leaking the answer through a translated
title. Queries cover recording music, making soup, coffee preferences, travel,
and bicycle repair, including cross-language questions.

| Mode | Expected memory ranked first | Degraded responses |
|---|---:|---:|
| Lexical | 2 / 6 | 0 |
| Semantic | 6 / 6 | 0 |
| Hybrid | 6 / 6 | 0 |

Lexical analysis does not translate languages. These are functional integration
probes, not an independent dataset, a broad recall guarantee, or evidence that
this model is best for every language/domain. The scale workload is unlabeled and
measures latency, not retrieval quality.

## Measured cache simplification

The first completed scale run used JSON for vectors. Its 10,000-record semantic
p50 was 1,363.65 ms. Profiling an isolated dense 10,000 × 768 vector cache showed
float-text parsing as a substantial cost. Switching only the disposable vector
cache to standard-library gob retained float64 values without quantization or a
new dependency. The corresponding final live semantic p50 was 363.43 ms.

[The prior live run](performance-before-binary-cache.json) and
[isolated codec measurements](cache-codec-comparison.txt) are retained. The isolated
generated-vector decode benchmark measured about 766 ms for JSON and 64 ms for gob;
it is a codec microbenchmark, not evidence of model quality. End-to-end hybrid
timings improved less and remain reported independently.

## Practical limits

- Exact vector ranking is linear in eligible chunks × dimensions; it is not ANN.
- Each operation scans directory metadata and reads the derived manifest. It does
  not reparse unchanged Markdown, but discovery/query cost grows with collection
  size. “Names-only” bounds context content, not filesystem work.
- A collection-wide lock serializes cooperating operations, including network
  embedding requests. Long provider calls can delay other operations in the same
  collection. Cancellation and HTTP timeouts are supported.
- Offset pages can shift if the collection changes between calls; there is no
  retained revision or snapshot system.
- External editors cannot be forced to honor locks or atomic-write durability.
  The last check/publication race with an uncooperative editor is documented.
- Crash tests exercise real process exit, not physical power removal. Durability
  relies on the documented fsync/atomic-publication contract and an honoring local
  filesystem.
- Only Linux was built and tested. No cross-platform claim is made.

Clara integration, existing-memory migration, publishing, and deployment are
outside this package's implementation scope.
