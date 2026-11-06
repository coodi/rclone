# Drive read baseline, 2026-11-06

Scope: this initial baseline exercises the backend directly. See [the VFS follow-up](drive-vfs-read-investigation.md) for `serve http`, full directory listing, 8 MiB ranges on 1.75.2 and the initial-seek patch. Statements below about unchanged production code describe the initial baseline only.

## Revision and build

- Fork branch: `codex/drive-read-performance`; starting commit `22b95b12f` adds investigation documentation to upstream `7c60060d4cb811fba5cf4e814207df4c7091d4df`.
- `git ls-remote upstream HEAD` returned that same upstream revision during this investigation.
- `go.mod` requires Go 1.26.0. Go 1.26.0 linux/amd64 built the full CLI successfully (`go build -o <scratch>/rclone .`); the binary reports `v1.76.0-DEV`, no build tags.
- Docker is present, but `/var/run/docker.sock` is owned by `root:docker` and the current user lacks access. No daemon, group membership, sudo packages, or production settings were changed. Docker execution remains unverified.
- Fallback: official Go archive extracted into a task-local scratch directory, with SHA256 verified against the Go release JSON manifest: `aac1b08a0fb0c4e0a7c1555beb7b59180b05dfc5a3d62e40e9de90cd42f88235`.
- Module versions and checksums remain exactly those in upstream `go.mod` and `go.sum`. `COPYING` and upstream attribution are unchanged.

## Existing capabilities

Source references are relative to the upstream revision above.

- `backend/drive/drive.go`: `NewObject` resolves parent directories through `lib/dircache`, then uses a name-filtered `Files.List` to resolve the file itself. This is not an unconditional listing of every file in the directory. Directory cardinality alone therefore does not establish linear lookup cost.
- Each Drive `Fs` retains its authorized HTTP client, Drive service, directory cache and pacer. Retaining that backend permits connection and directory-cache reuse. A fresh process cannot retain those in-memory objects.
- A regular `Object` stores the provider ID and media URL. Repeated `Open(ctx, &fs.RangeOption{...})` sends media requests without another path lookup or metadata GET. The default `v2_download_min_size=-1` is used in this fixture; Google documents and optional v2 downloads take different paths.
- Drive already has `backend copyid`/`moveid`: `copyOrMoveID` calls `getFile`, then `newObjectWithInfo`. `copyid` copies by ID, including server-side copying where applicable; it is not a public ranged-read endpoint. `backend get` reads backend options, not an object by ID. `backend query` returns query results, not a ranged media stream. A folder's `root_folder_id` likewise is not an arbitrary file-ID read API.
- `fs/cache` already caches backend instances. `fs/rc/rcserver/rcserver.go:serveRemote` uses `cache.Get` but still calls `NewObject` per HTTP file request. A persistent RC process can retain its backend without eliminating that final file lookup.
- The benchmark's ID case uses existing package-private functions. It demonstrates an existing internal capability; it does not introduce a CLI, RC endpoint or exported Go interface.

## Synthetic method

`backend/drive/read_latency_internal_test.go` runs a loopback `httptest` server and the real Drive lookup/Open implementation. No OAuth, credentials, external cloud, user file names, file IDs or private application data are used. Module/toolchain downloads and the public upstream revision lookup are the only external services needed for setup.

The matrix uses directory depths 0, 4 and 16; artificial delay 0 or 2 ms per request; alternating offsets 0 and 1 MiB; and an exact 8192-byte range from a synthetic 1 GiB object. It records list calls, metadata GETs, media calls, actual media bytes, allocations, total operation duration, lookup-to-first-byte and Open-to-first-byte. Every iteration drains and closes its bounded response. The unit test verifies cold lookup, warm directory cache, repeated seek reads, the existing ID metadata path, byte counts and pre-canceled context propagation.

Modes:

- `cold-path`: create a fresh minimal backend and resolve the full path for each range.
- `warm-path`: retain the backend and resolve the file path for each range.
- `id-metadata`: retain the backend, GET metadata by ID, construct the object and read.
- `reuse-object`: retain the already resolved object and open another range.

The fixture explicitly provides a known synthetic root, disables document export and optional v2 download, and shares the loopback client's transport even between fresh backend structs. Thus “cold” means an empty directory cache, not a fresh process, TCP connection, OAuth session or fully constructed `NewFs`. Benchmark setup warms the shared connection and is excluded from metrics. Pacing uses a large burst and zero minimum sleep to isolate lookup cost; it does not simulate Google throttling.

## Results

Medians of three `-benchtime=100ms` runs on linux/amd64, Intel Core i7-13700, 24 logical CPUs. Full CLI compilation finished before the reported benchmark run. Times are illustrative local measurements; request counts are the reproducible result. All modes transferred exactly 8192 media bytes and made one media request per operation.

| Depth | Mode | List/op | Metadata GET/op | Total ms/op, 2 ms injected delay |
|---:|---|---:|---:|---:|
| 0 | cold-path | 1 | 0 | 4.740 |
| 0 | warm-path | 1 | 0 | 4.867 |
| 0 | id-metadata | 0 | 1 | 4.855 |
| 0 | reuse-object | 0 | 0 | 2.412 |
| 4 | cold-path | 5 | 0 | 14.769 |
| 4 | warm-path | 1 | 0 | 4.678 |
| 4 | id-metadata | 0 | 1 | 4.753 |
| 4 | reuse-object | 0 | 0 | 2.335 |
| 16 | cold-path | 17 | 0 | 42.967 |
| 16 | warm-path | 1 | 0 | 4.714 |
| 16 | id-metadata | 0 | 1 | 4.897 |
| 16 | reuse-object | 0 | 0 | 2.326 |

Open-to-first-byte was approximately 2.29–2.38 ms across these cases. The extra cold-path cost occurs before Open. ID metadata lookup avoids parent traversal but does not remove the one metadata round trip compared with a warm path. These results do not establish a Google Drive speedup or reproduce the historical tens-of-seconds delay.

## Reproduction

From the checkout, set `GO_BIN` to the absolute Go 1.26.0 binary and `SCRATCH` to a task-owned directory outside tracked source. Fetch dependencies once with `go mod download` using the intended public module proxy. The measurement itself runs offline apart from loopback. Use dedicated caches already populated during setup.

```sh
mkdir -p "$SCRATCH/home" "$SCRATCH/gopath" "$SCRATCH/gocache"
# Run dependency setup with these same GOPATH and GOCACHE paths first.
env -i PATH=/usr/bin:/bin HOME="$SCRATCH/home" \
  GOTOOLCHAIN=local GOPATH="$SCRATCH/gopath" GOCACHE="$SCRATCH/gocache" \
  GOPROXY=off RCLONE_CONFIG=/notfound \
  "$GO_BIN" test ./backend/drive -run '^$' \
  -bench '^BenchmarkDriveReadLatency$' -benchmem -benchtime=100ms -count=3

env -i PATH=/usr/bin:/bin HOME="$SCRATCH/home" \
  GOTOOLCHAIN=local GOPATH="$SCRATCH/gopath" GOCACHE="$SCRATCH/gocache" \
  GOPROXY=off RCLONE_CONFIG=/notfound \
  "$GO_BIN" test ./backend/drive ./lib/dircache ./fs/cache -count=1

env -i PATH=/usr/bin:/bin HOME="$SCRATCH/home" \
  GOTOOLCHAIN=local GOPATH="$SCRATCH/gopath" GOCACHE="$SCRATCH/gocache" \
  GOPROXY=off RCLONE_CONFIG=/notfound \
  "$GO_BIN" test -race ./backend/drive -run '^TestDriveReadLatencyBaseline$' -count=1
```

The clean environment intentionally excludes any inherited remote configuration or credentials. The full build and targeted unit packages passed. The complete `make quicktest`, real-cloud integration tests and comparison against rclone 1.60.1 were not run.

## Smallest justified optimization

First use an existing persistent backend and retain the resolved `Object` for successive ranges within one logical read session. This is already supported internally: the synthetic test verifies zero additional metadata/list requests on subsequent Opens. Keep request contexts and close each range body. End the session before resolving a new path; avoid introducing a global path-to-object cache with stale rename/delete/permission semantics.

If a public ID read interface is needed after caller profiling, a small opt-in Drive-specific backend command that reuses `getFile` and `newObjectWithInfo` is a candidate for a separate design and regression test. It should not reinterpret existing paths as IDs or modify generic fs/vfs behavior. It is not implemented here: this baseline has not established a production-code defect requiring a fix.

## Remaining evidence needed

Before selecting a source patch, extend the fixture with paginated duplicate names, large synthetic directories honoring the actual name filter, moved/deleted IDs, shortcuts/resource keys and shared drives. Add in-flight cancellation, concurrent ranges and large-body bounded-memory tests for any new read/session implementation. Measure connection acquisition separately with `httptrace`, plus full `NewFs`, process startup, simulated token refresh and controlled retry/pacing errors. The current loopback test has no DNS, TLS, real OAuth refresh, provider latency, network bandwidth or HTTP/2 behavior.

Real-cloud validation requires a separately authorized synthetic test account. Do not infer that a real 8 MiB read will match these 8 KiB local timings. No upstream PR or production change is part of this stage.
