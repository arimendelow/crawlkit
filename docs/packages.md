# Package guide

`crawlkit` keeps reusable archive mechanics in small Go packages. Downstream apps compose these packages while retaining provider APIs, schemas, authentication, privacy policy, and CLI compatibility.

## Local data

- `config` provides TOML loading, standard config paths, opt-in platform-native runtime directories, legacy-path fallback, and token diagnostics.
- `store` provides SQLite open, read-only, transaction, query, schema-version, FTS5 term, and optimization helpers.
- `state` provides generic crawler cursors and freshness records, including mapped adapters for existing app table layouts.
- `cache` provides read-only local cache files and staged SQLite database, WAL, and SHM captures. Callers must supply a quiescent source or an application-owned coherent snapshot; copying files from a live writer does not provide a transactional snapshot.

`cache.SnapshotFile` accepts a single filename in `Name`, preserving its literal
text, including whitespace. An empty name uses the source basename. Paths and
parent-directory components are rejected before copying so captures stay inside
`CacheDir`.

A positive `MaxFileBytes` bounds each captured source file, including SQLite
sidecars. Nonpositive limits leave file size unbounded.

## Portable archives

- `snapshot` exports and imports manifest-based JSONL/Gzip table packs, fingerprints files, plans exact or monotonic incremental imports, and synchronizes managed sidecar trees.
- `backup` writes age-encrypted JSONL/Gzip shards and manifests, manages recipients and identities, lists Git-backed history, and verifies historical restores.
- `mirror` clones, initializes, pulls, commits, and pushes Git-backed archives. It also provides non-mutating fetches, immutable snapshot tags, Git-object reads, and history inspection.

Managed sidecar trees reject overlapping source and destination roots, including
mixed relative and absolute paths. Configured root symlinks remain supported;
symlink directories inside a destination copy path are rejected so copying and
pruning agree on the files retained. Destination writes and pruning stay rooted
in the selected target directory. Source, snapshot-root, and target directory
names retain literal leading and trailing whitespace, just like shard paths.
On Windows, directory components with trailing spaces or dots are rejected
before copying or pruning because Win32 can alias them to other names.

Snapshot exports use `tables/.generations/<32 lowercase hex>/<table>/<ordinal>.jsonl.gz`,
where the ordinal has at least six digits. Manifest fields are unchanged;
planners match these paths to legacy `tables/<table>/<ordinal>.jsonl.gz` IDs.
Unchanged physical shards are reused only after comparing actual bytes.
Old literal readers can consume the paths; old planners may request replacement.
Strict downstream publication validators must explicitly admit this form.

Export holds `.crawlkit-snapshot.lock`, stages closed/synced shards, and publishes
the manifest last. Failures before promotion retain the prior pack. Cleanup
deletes exact prior managed files only; unlisted files and directories are not
recursively owned, and empty generation directories may remain. Post-promotion
cleanup errors return the committed manifest. Readers must coordinate with
pruning; this is not a read lease or a power-loss durability guarantee.

All table reads use one transaction. `ReadTx` optionally borrows a caller's
transaction without ending it, even on error. `FilterTx` runs after the legacy
filter for admitted rows, using that same transaction; it must only read and
must not commit or roll back. Legacy filter closures retain their own database
bindings. The driver's `ReadOnly` option is not an authorization boundary for
trusted callbacks.

Snapshot rows remain v1 JSON objects; no new wire format is enabled. Import
planning compares unique column names independently of their order, so a
snapshot from an equivalent schema does not require replacement just because
its columns were created in a different order. Comparisons with added, removed,
renamed, or duplicate columns still require replacement; shard and fingerprint
checks are unchanged. Original manifest column order is preserved.

Import callbacks still receive ordinary numbers as `float64`. Exact integral tokens
outside +/- (2^53-1), including decimal/exponent spellings, become `int64` when
they fit; no `json.Number` escapes to callers. Integers outside signed 64-bit
range, oversized fractional magnitudes, overflow, nonzero underflow, numeric
tokens over 4096 bytes and invalid Unicode fail transactionally. Ordinary
fractional values retain float64 rounding.

V1 export refuses admitted BLOBs and integers outside +/- (2^53-1), including
integral-looking REALs, before publishing the manifest. It also refuses invalid
UTF-8 and numbers the reader cannot represent. Excluded rows do not fail.
Filters retain the legacy BLOB-as-string input and may remove unsupported cells
or replace them with an explicit supported representation, such as prefixed
base64 text; an unchanged implicit BLOB string is not sufficient. Explicit
custom JSON/text encoders own their transformation. The prior pack survives a
refusal. This prevents silent loss, not full BLOB export support or recovery of
binary/text distinctions already lost by old writers.

Generic incremental deletes and `INSERT OR REPLACE` refuse inbound cascading,
`SET NULL` or `SET DEFAULT` foreign keys and triggers on affected tables before
`BeforeImport` runs. Otherwise skipped/unlisted tables can lose local history.
The generic guard supports unshadowed main-schema tables and includes temporary
triggers. Restrictive foreign keys keep normal transactional failure behavior.
Dependency-aware custom `DeleteTable` and `ImportRow` callbacks retain their
contract and own the safety of their writes; hooks must not invalidate the
checked schema. Full import behavior is unchanged.

Encrypted backup writers hold `.crawlkit-backup.lock` through publication and
cleanup. This persistent local marker is not manifest-owned; never unlink it
to release a writer. Publish only exact current/prior manifest paths when an
app stages a backup for Git. Generic `mirror.Commit` remains unchanged.

New encrypted shards and file indexes use unique physical names; legacy
logical names and manifest fields remain readable. The manifest is published
last. A failure before publication preserves the prior pack. A cleanup error
after publication returns the committed manifest and an explicit cleanup
error. Cleanup removes only exact prior manifest-owned objects, not unrelated
files. Callers must coordinate readers with pruning; this is not a concurrent
reader lease or a power-loss durability guarantee.

## Search

- `embed` provides OpenAI-compatible, Ollama, and llama.cpp embedding clients plus probe diagnostics.
- `vector` encodes float32 vectors, validates dimensions, runs exact cosine or optional turbovec-backed search, selects top-k results, and performs reciprocal-rank fusion.

Embedding request timeouts also apply to custom HTTP clients. Providers copy
the client settings while sharing its transport, redirects, and cookie jar;
shorter client or context deadlines still take precedence.

## Chromium app caches

Chromium-based desktop apps (Electron and WebView2 apps such as Teams, Slack, and Discord) keep state in IndexedDB on LevelDB, with values in V8's structured-clone format. These pure-Go packages read a copied profile directory without Node, cgo, or a running app. They originate in [ourostack/teamscrawl](https://github.com/ourostack/teamscrawl) and know nothing about any one app's databases; callers own which databases to read, what the records mean, and privacy policy. Callers must read a copy, never the live profile.

- `chromium/leveldb` loads a LevelDB directory. The MANIFEST decides which tables and logs are live, the highest sequence number wins per key, and a log or manifest tail cut off by a live writer is tolerated and counted in `Stats`. `LoadWith` can drop keys before their values are held and re-reads large table values on demand.
- `chromium/indexeddb` reads an origin's LevelDB plus blob directory: databases and object stores, record keys, Blink value envelopes (plain, Snappy, v21 trailer, external blob), and external blob files. Undecodable values are typed `OmissionError` codes, not failures. `OpenWith` accepts `KeepDatabase`/`KeepStore` filters so unwanted databases are never held, and `Census` estimates per-database memory before opening.
- `chromium/v8` deserializes V8 wire versions 13 to 16 into plain Go values (objects, arrays, maps, sets, dates, BigInts, errors, typed buffers, shared references) and renders them as canonical JSON. Host objects and unknown tags return typed errors.

The V8 test vectors under `chromium/v8/testdata/vectors` are synthetic and generated by `chromium/v8/testdata/gen/gen.mjs` on Node 22; `TestGeneratorIsCurrent` checks them when Node 22 is installed (set `CRAWLKIT_REQUIRE_NODE=1` to fail instead of skip). The IndexedDB fixture is built in a temp directory by the tests. No real app data is committed.

## App contracts

- `control` defines crawler metadata, command manifests, status payloads, contact exports, and database inventories for launchers and automation.
- `output` writes text, JSON, and log-oriented command output.
- `progress` provides progress logging that stays readable in terminals and CI logs.

## Remote archives

- `remote` provides a provider-neutral HTTP client, configuration, query, ingest, authentication, status, SQLite bundle, and protocol-contract types for Worker-fronted archives.

The service boundary is defined in [Remote Contract](remote-contract.md). The Cloudflare Worker and D1 deployment remain outside this module.

## Background processing

- `worker` runs generic batched handlers over app-owned durable queues with revision fencing, expiring leases, priority, retries, cancellation, and status. See [Background workers](background-workers.md).

## User surfaces

- `scheduler` discovers crawl apps, expands job config, prevents concurrent runs, records JSONL history, and renders or installs native schedules.
- `tui` provides the shared terminal archive explorer: responsive panes, entity and member lists, details, sorting, filtering, mouse actions, and local or remote source status.
- `releasecheck` checks GitHub Releases, caches results, suppresses notices for scripted output, and formats update messages for downstream CLIs.

## Command

- `cmd/crawlctl` is the controller CLI built on `scheduler`. It discovers installed crawl apps through `metadata --json`, runs configured jobs, reports status and logs, and manages periodic schedules.

Browse the exported APIs in the [Go package reference](https://pkg.go.dev/github.com/openclaw/crawlkit).
