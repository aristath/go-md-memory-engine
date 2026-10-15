# go-md-memory-engine

A standalone Go library and JSON CLI for memories stored in Markdown. Files hold
the complete collection; an embedded Bleve index and embedding cache accelerate
retrieval. No LLM is required to store, query, validate, or rebuild memories.

Supported platform: Linux, Go 1.27+. No database service, daemon, CGO, GPU runtime,
or model download is required. An optional HTTP embedding service enables semantic
and hybrid recall. This package has no dependency on Clara.

## Build and use

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -o memory ./cmd/memory
./memory -root /path/to/my-collection create <<'JSON'
{
  "title": "A recording session",
  "body": "We recorded a song together.\n",
  "taxonomies": {
    "people": [{"type":"text","text":"Dimitra"}],
    "event_time": [{"type":"time","time":{"date":"2008"}}]
  },
  "sources": [{"conversation_id":"example","message_id":"123"}]
}
JSON
./memory -root /path/to/my-collection taxonomies
```

Install the CLI or import the Go library from its independent repository:

```sh
go install github.com/aristath/go-md-memory-engine/cmd/memory@latest
go get github.com/aristath/go-md-memory-engine
```

```go
import memory "github.com/aristath/go-md-memory-engine"
```

See [the executable public API example](example_test.go), [design](DESIGN.md),
[measured results](docs/verification.md), and
`go doc github.com/aristath/go-md-memory-engine`.

## Document format

```markdown
---
{
  "format": 1,
  "id": "recording-2008",
  "title": "A recording session",
  "created_at": "2026-10-15T10:00:00Z",
  "updated_at": "2026-10-15T10:00:00Z",
  "taxonomies": {
    "people": [{"type":"reference","ref":"person-dimitra"}],
    "activities": [{"type":"text","text":"recording"}],
    "event_time": [{"type":"time","time":{"start":"2008","end":"2010","approximate":true}}]
  },
  "links": [{"relation":"part-of","target":"album-one"}],
  "sources": [{"conversation_id":"example","message_id":"123"}],
  "extra": {"caller_defined_information":"kept here"}
}
---
# Original account

Αυτό είναι το αρχικό κείμενο.

# Additional description

The caller decides the structure and size of the account.
```

Frontmatter is JSON enclosed in `---` lines: a YAML-compatible subset with a
standard-library parser, not arbitrary YAML. Unknown envelope fields are errors;
use `extra` for arbitrary metadata. Taxonomy names and their meanings are open.

`Create` supplies format, a random 128-bit hex ID when absent, and timestamps.
A caller may provide an ID and creation timestamp. IDs contain 1–128 ASCII
letters/digits/dots/underscores/hyphens and start with a letter or digit.
Titles are nonempty single-line strings of at most 256 bytes. Manual documents
must include format, ID, title, and valid RFC3339 timestamps.

Engine-created documents live at `records/<id>.md`. Existing valid documents can
live anywhere below the collection root except reserved `.memory/`. Filenames
are not identities; moving or renaming a file preserves its ID. Close a Store
before moving its collection directory. Copy all Markdown files, keeping their
relative layout if convenient, to move a collection; regenerate `.memory/`.

The body starts immediately after the closing delimiter's newline. Its UTF-8
bytes, line endings, whitespace, fences, and headings survive reads and metadata
updates exactly. The engine does not render/reformat Markdown. Metadata is
canonically re-encoded on engine writes; original frontmatter formatting is not
preserved. Sources are opaque JSON objects; numeric source identifiers retain
JSON number precision. The engine does not verify external chat references.

## Values and taxonomies

There is one map, `taxonomies`, from arbitrary names to arrays of typed values.
This supports categories and structured attributes without separate registries.

Supported values:

```json
[
  {"type":"text","text":"Dimitra"},
  {"type":"number","number":3.5},
  {"type":"boolean","boolean":false},
  {"type":"reference","ref":"person-dimitra"},
  {"type":"time","time":{"date":"2008-06"}},
  {"type":"list","items":[{"type":"text","text":"music"}]}
]
```

Numbers use finite IEEE-754 float64 values. A list may be empty, nests to at most
16 levels, and retains its order. Query equality can match a whole list or an
individual nested leaf. Taxonomy entries are the directly assigned values, so
an explicitly assigned list is one entry; its leaves remain queryable.

Surrounding whitespace in taxonomy names, text values, and relation names is
trimmed. Otherwise names/values are case-sensitive and distinct. No fuzzy
identity merging, synonym inference, transliteration, or semantic deduplication
occurs. Assignments identical after this normalization are deduplicated within
the memory. Two taxonomy keys colliding after trimming are rejected.

### Supplying current names to an LLM

Call `Store.Taxonomies(ctx)` during application-side context preparation:

```json
{"names":["activities","event_time","people"],"revision":"content-hash"}
```

Insert only `names` into each model request. `revision` hashes the sorted names,
so a client can cheaply compare/cache its context block; entry-only changes do
not invalidate it. The call reconciles external edits before answering, scanning
file metadata and reading the derived record manifest. Its collection-size-dependent
cost is measured in the verification report. There is no
model-issued discovery call and no requirement to retain names in chat history.

Entries are opt-in, e.g. `Store.Entries(ctx,"people","dim",Page{Limit:20})`:

```sh
echo '{"name":"people","search":"dim","limit":20}' |
  ./memory -root /path/to/my-collection entries
```

Each entry reports its typed value and number of distinct matching memories.
Names arise from valid memories, including explicitly empty assignments. Removing
the last assignment removes that taxonomy naturally. Invalid or duplicate-ID
documents are diagnosed and excluded, so their names are not presented as valid.

## Structured queries

```sh
./memory -root /path/to/my-collection query <<'JSON'
{
  "all":[{"taxonomy":"people","op":"eq","value":{"type":"text","text":"Dimitra"}}],
  "any":[
    {"taxonomy":"activities","op":"eq","value":{"type":"text","text":"recording"}},
    {"taxonomy":"activities","op":"eq","value":{"type":"text","text":"rehearsal"}}
  ],
  "sort":[{"field":"created_at","desc":true}],
  "limit":20,
  "offset":0
}
JSON
```

All `all` conditions and at least one `any` condition must match. An empty group
adds no restriction. Optional `ids`, `kind`, and `text` filters combine with both
groups. `text` uses the same full-text analyzer as recall.

Operators: `eq`, `ne`, `exists`, `not_exists`, case-insensitive text `contains`,
numeric `gt/gte/lt/lte`, and temporal `overlaps/before/after`. Existence operators
take no value. `ne` means none of the assigned values equal the supplied value,
including a missing taxonomy. Numeric comparisons require numeric values.

Exact/existence/numeric filters use the inverted index for candidates. Substring
and temporal predicates refine indexed taxonomy candidates against cached typed
metadata; no Markdown parsing or model call is involved. A broad query can
naturally inspect many candidates.

Sort by `id`, `title`, `created_at`, `updated_at`, or `taxonomy:<name>`. Timestamps
sort chronologically, numeric first-values numerically, and other first-values
by canonical JSON. Missing taxonomy values sort before present values in ascending
order. ID always breaks ties. Default order is ID.

`limit` defaults to 20; positive larger limits are supported without a fixed cap.
`total` counts matched records before pagination; `next_offset` is present when
more remain. `exhaustive` is true only for structured results with no collection
diagnostics or excluded uncertain matches. It describes the whole paginated
result set, not whether the current page contains everything.

Each operation sees the current synchronized collection. Offset paging is stable
for an unchanged collection; edits between requests can shift pages. No historical
snapshots are retained.

Results contain ID, title, relative path, current hash, matching conditions,
certainty, sources, and a bounded excerpt, not complete memories. Excerpt line
numbers are **body-relative**, beginning at 1. Use Get/GetMany for full records.

### Temporal semantics

Use `date` for YYYY, YYYY-MM, or YYYY-MM-DD. Use inclusive `start`/`end` for a range;
one missing bound is open. `date` cannot coexist with range bounds. Set
`unknown:true` without bounds for an undated memory, with an optional description.
`approximate:true` marks nonbinding estimates. Original fields are preserved.

Calendar bounds describe possible event times, not an assertion that an event
lasted for the entire interval. For `overlaps`, all possible times inside the
query interval give a definite match; partial intersection is possible; disjoint
known intervals do not match. Thus “2008” is definite within “2008–2010”, but
only possible within “June 2008”. Before/after are definite only when every
possible pair satisfies the comparison; ambiguous order is possible.

Unknown dates produce unknown comparisons. Approximate matching bounds produce
possible results; a comparison outside those bounds remains unknown, because
the estimate does not justify exclusion. There is no invented uncertainty margin.

By default, only definite matches are returned. `uncertain_excluded` reports
possible/unknown records omitted by this policy. Set `include_uncertain:true`
to include them with their certainty labels. No calendar expansion is written
back into the source document.

## Recall and embeddings

```sh
echo '{"text":"recording a song","mode":"lexical","limit":10}' |
  ./memory -root /path/to/my-collection search

export MEMORY_EMBEDDING_BASE=http://127.0.0.1:18200/v1
export MEMORY_EMBEDDING_MODEL=your-multilingual-embedding-model
# Optional secret, read only from the environment by the CLI:
# export MEMORY_EMBEDDING_API_KEY=...
echo '{"text":"recording a song","mode":"hybrid","limit":10}' |
  ./memory -root /path/to/my-collection search
```

Modes are lexical (default), semantic, and hybrid. Recall is ranked, always
`exhaustive:false`. `total` is the number of ranked candidates, not proof of
complete factual coverage. `filter` accepts the structured Query shape; its
pagination is ignored in favor of recall's own pagination. Temporal uncertainty
policy still applies.

Lexical search uses Bleve BM25 with Unicode tokenization, lowercase, English
and Greek stopword removal, English Porter stemming, and title boosting. This
is not a Greek stemmer, transliterator, or cross-language translator. Exact
taxonomy matching is independent of the analyzer. Indexed text includes titles,
bodies, and taxonomy values/names, not opaque sources or extra metadata.

Semantic recall uses cosine similarity; `min_similarity` optionally limits its
candidate channel to [-1,1]. Hybrid uses reciprocal-rank fusion with constant 60.
Scores are mode-specific, not probabilities. ID breaks score ties.

Long records are split into 2,000-rune chunks with up to 200 runes of overlap.
The best chunk determines a memory's semantic score. Semantic excerpts come from
embedding chunks (which can include title/metadata), so their body-line fields
are 0. `-embedding-chunk-runes` adjusts chunk size to the model's input capacity;
runes are not tokenizer tokens. Oversized-provider input errors are explicit.

Documents are embedded on demand, in batches. Vector keys include search content,
endpoint, model, requested dimensions, and chunking. Changes/deletions invalidate
affected vectors; switching model or relevant settings re-embeds eligible records.
Credentials, batching, and timeout changes do not change embedding identity.
The CLI also accepts `-embedding-dimensions`, `-embedding-batch-size`, and
`-embedding-timeout`. Adjust batch size to the provider's capacity. Change the
model identifier or explicitly rebuild if a provider replaces weights under an
unchanged identifier; the engine cannot discover that change from the API name.

Vectors use Go's standard-library gob encoding in a separate disposable cache.
Ordinary reads, taxonomy discovery, and lexical/structured queries do not load it.
Its encoding can change without changing the authoritative Markdown format.

Semantic ranking is exact over cached vectors, O(eligible chunks × dimensions).
There is no native ANN dependency. Indexing does not require network access.
If embedding generation or querying fails, recall returns lexical results with
`mode:"lexical"` and an explicit `degraded` reason. Deterministic operations stay
available. Provider error bodies and API keys are not included in diagnostics.

The collection lock is held for the operation, including embedding requests.
This simple consistency model serializes cooperating operations; a slow provider
can delay other users of that collection. Timeouts and cancellation bound waiting.
Use separate collections for independent workloads.

## Entities and links

An entity is an ordinary record with a caller-defined `kind`, such as `person`
or `place`. Its ID is the reference target. Multiple people named Dimitra can
coexist; the engine never merges them by name.

`links` holds `{relation,target}` pairs. Reference-valued taxonomy assignments
also appear as links with relation `taxonomy:<name>`. Outgoing and incoming links
are computed from the current collection. Missing targets remain explicit with
`resolved:false` and a diagnostic, not fabricated placeholder records.

`Links(ctx,id,GraphOptions)` returns one hop. `Traverse` uses breadth-first
traversal to the requested depth (default 1), with cycle detection, optional
relation filtering and direction `outgoing`, `incoming`, or `both` (default).
Edges sort by source/relation/target, then paginate. Nodes are the starting ID
and endpoints of returned edges. Traversal depth bounds exploration; edge paging
bounds output, not total traversal work.

## CLI/API operations

Flags go before the command. Input is one JSON value on stdin; successful output
is JSON on stdout, errors are JSON on stderr. Exit codes: 1 general/input error,
3 not found, 4 stale-write conflict. Context cancellation handles SIGINT/SIGTERM.

| Command | Input | Go method |
|---|---|---|
| create | Record (format/ID/timestamps may be omitted) | Create |
| get | `{ "id":"..." }` | Get |
| get-many | `{ "ids":["..."] }` | GetMany |
| update | `{ "record":{...}, "expected_hash":"..." }` | Update |
| patch | `{ "id":"...", "expected_hash":"...", "changes":{"title":"New title"} }` | Patch |
| delete | `{ "id":"...", "expected_hash":"..." }` | Delete |
| query | Query | Query |
| search | Recall | Search |
| taxonomies | none | Taxonomies |
| entries | `{ "name":"people", "search":"dim", "offset":0, "limit":20 }` | Entries |
| links / traverse | `{ "id":"...", "direction":"both", "depth":2, "limit":20 }` | Links / Traverse |
| sync | none | Sync(ctx,false) |
| check | none | Sync(ctx,true) |
| rebuild | none | Rebuild |
| stats | none | Stats |

Get/GetMany return full records and hashes. Missing batch IDs are listed separately.
Full reads read and validate the selected authoritative files rather than trusting
the cached body. An external edit racing that read is reported for retry.
Update replaces the supplied record's content and metadata, preserves its original
creation timestamp, and updates its modification timestamp. Omitted fields are
removed; read first when making a metadata-only change. Update/Delete require the
hash from a current read. There are no historical revisions or tombstone records.

Use Patch to change metadata without retrieving or transmitting the body. It
applies JSON Merge Patch semantics: objects merge recursively, arrays replace,
and null removes fields. Only title, kind, taxonomies, links, sources, extra, and
body can be patched. The expected hash can come from a compact query hit. An
omitted body remains byte-identical; an explicit empty body clears it. A concurrent
change between the internal read and write is rejected by the same hash check.

## Filesystem guarantees and recovery

All operations reconcile file metadata under a collection-wide POSIX `flock`.
New/changed files are parsed; unchanged files are not. Linux device/inode, size,
mtime, and ctime detect replacements and edits even if an editor restores mtime.
`check` additionally hashes all files. Notifications are unnecessary, so correctness
does not depend on a watcher, a long-running process, or receiving every event.

Writes create a same-directory private temporary file, flush it, publish by
exclusive hard link (creation) or atomic rename (replacement), and flush the
parent directory. Delete flushes the parent directory after unlinking. Created
record files use 0600 and record/cache directories 0700. Private parent directories
protect the index library's own file modes.

An acknowledged mutation means its Markdown publication/deletion completed these
durability steps on a local filesystem honoring fsync and atomic rename/link.
No guarantee is made for filesystems/hardware that do not honor them. Sources are
authoritative even if subsequent derived-index maintenance fails: the receipt
contains the durable ID/path/hash and a repair warning rather than returning an
ambiguous “save failed” that invites a duplicate.

A durable `.memory/dirty` marker precedes mutation. An interrupted operation
triggers rebuilding on the next access. Invalid/missing index or malformed manifest
also triggers reconstruction. Explicit rebuild reparses source files and clears
vectors. Recovery never rewrites source documents. Unacknowledged interruption
can leave an old or new complete record, or a staging file; there is no partial
acknowledged record. Staging files do not end in `.md` and are not memories.

Cooperating readers and writers are serialized across Store instances and CLI
processes. Conflicting updates are rejected by content hash, with a second check
immediately before publication. Ordinary external editors do not honor the lock:
an edit occurring in the final check/rename gap can still race. The package cannot
provide transactions against an uncooperative writer. External editors also own
their own write durability. A file changing during synchronization is reported
for retry rather than publishing an inconsistent snapshot.

Malformed files and every copy of a duplicate ID are excluded with diagnostics.
Broken links are reported without excluding their source records. Get errors can
carry a CollectionError with diagnostics; successful discovery/query/receipt
responses also expose collection problems. Check's nonempty diagnostics are a
result to inspect, not an automatic nonzero process exit.

Corpus access uses `os.Root`, rejects symbolic links and nonregular Markdown
files, and refuses symlinks in the private index tree. IDs never become arbitrary
paths. Keep collection roots and `.memory/` controlled by the trusted application;
OS-level mount manipulation or a same-user adversary concurrently replacing the
index/lock tree is outside this boundary. Authentication belongs to the caller,
which chooses an authorized collection root before invoking the engine.

Do not delete the coordination lock or cache directory while any process is using
the collection. Close clients before removing the entire cache. `rebuild` performs
an online coordinated rebuild without replacing the lock inode.

## Verification and measurement

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build ./...
go test -run '^$' -fuzz '^FuzzParseDoesNotPanic$' -fuzztime 10s -parallel 4
go run ./cmd/memory-measure -sizes 100,1000,10000 -samples 10
go run ./cmd/memory-measure -quality-only \
  -embedding-base http://127.0.0.1:18200/v1 -embedding-model YOUR_MODEL
```

The measurement command uses only temporary collections and removes them. Scale
tests use real Go documentation text; quality probes use explicitly hand-authored
Greek/English accounts. See the committed report for measurements and limitations.
