# Markdown memory engine

A standalone Go library and JSON CLI. No Clara integration or live-data migration.

## Files and values

A collection is an explicit directory. Records are Markdown with JSON frontmatter
between `---` lines, followed by the exact body bytes. JSON is unambiguous,
standard-library parsable, and a YAML subset. A small envelope holds format, ID,
title, optional entity kind, timestamps, source objects, taxonomy assignments,
and typed relationships. Taxonomies are arbitrary names mapped to typed values:
text, number, boolean, reference, time, and list. Entities are ordinary records
with a kind, so their identities and links survive copying the files alone.
No revisions, implicit semantic merging, or generated enrichment.

Temporal values preserve dates at year/month/day precision, explicit ranges,
open bounds, approximation, unknown dates, and descriptive uncertainty. Query
comparisons report definite/possible/unknown; uncertain matches are opt-in and
reported separately. Calendar bounds used for comparisons never replace source
values.

## Public operations

Go API and thin JSON CLI: CRUD and batch reads; structured conjunctive/disjunctive
queries, sorting and paging; taxonomy names with a content-derived revision;
explicit paginated entries; lexical, semantic and hybrid search; links/backlinks,
bounded traversal; sync/check/rebuild/stats. Updates/deletes require the current
content hash to reject stale writers. Sources remain opaque structured objects.
Results contain compact excerpts, IDs, sources, and explicit continuation data.

## Indexes

Bleve supplies an embedded, persistent inverted index for text and typed exact/
numeric filters. A derived manifest caches parsed records and file fingerprints.
Each operation reconciles directory metadata; unchanged records are not reparsed.
Changes update only affected index documents. No filesystem watcher or daemon is
necessary: directory reconciliation also handles missed notifications and new
CLI processes. Explicit full verification hashes every record.

Embeddings use an optional OpenAI-compatible HTTP endpoint. Cached vectors are
keyed by record search content and provider/model/dimension configuration.
Semantic search computes exact cosine ranking over cached vectors; hybrid search
uses reciprocal-rank fusion. This avoids a native ANN dependency and gives exact
results; its O(N*d) cost is measured and documented rather than hidden. Embedding
failure leaves deterministic operations usable and marks recall degraded.

## Writes and recovery

A private cache directory holds a process-coordination lock, Bleve index, manifest,
and disposable vector cache. Cooperating operations are serialized using a file
lock. Corpus access uses Go os.Root; symlinks are rejected. Files and directories
use private permissions. Publication flushes a same-directory temporary file,
renames it atomically, then flushes its parent directory. Deletes also flush the
parent. Success acknowledges durable Markdown publication; derived-index errors
are reported as recoverable warnings rather than inviting duplicate creation.

A dirty marker precedes file/index mutations. The next operation rebuilds derived
state after an interrupted mutation. Rebuilds never alter source documents.
Malformed records, duplicate IDs and symlinks are diagnosed, never rewritten;
invalid/ambiguous records are excluded with explicit diagnostics. Rename detection
preserves identity. External editors cannot be forced to honor locks or durability;
read-before-write hash checks and prepublication rechecks detect observed conflicts,
while the final race with an uncooperative external editor is documented.

## Focused dependencies

Bleve earns its dependency cost through persistent indexing, filters, and ranking.
The standard library handles JSON frontmatter and filesystem operations. Markdown
is stored verbatim; section excerpts can use Goldmark if heading-aware retrieval
requires it. fsnotify adds no correctness here. renameio explicitly does not promise
power-loss durability, so the small publication sequence remains ours. POSIX file
locking is used directly; the initial supported target is Linux (the user's host).
