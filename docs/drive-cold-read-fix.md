# Cold Drive opening: localization and opt-in direct reads

## What the authorized real test established

An isolated read-only test of diagnostic revision `5721f24e05eee7977af421d685d8f68c1884ac88` localized the remaining cold delay to VFS lookup/preparation before media. This section contains only the aggregate results supplied by that test. No private files, configurations, names, URLs, provider IDs, content hashes, addresses or raw traces were imported into the fork. Production stayed on official 1.75.2.

Two fresh persistent `serve http` processes used the same ordinary object, route and settings: disk VFS cache off, directory cache five minutes, buffer 1 MiB, chunks 8 MiB. GET ranges started at 8 MiB and had length 8 MiB.

| Measurement | Process A: GET first | Process B: HEAD first |
|---|---:|---:|
| Startup / listening | 1.611 s | 1.686 s |
| First GET headers | 33.140 s | 0.988 s, after HEAD |
| First GET complete | 34.746 s | 2.681 s, after HEAD |
| First HEAD | not requested | 33.791 s, zero body bytes |
| Repeated GET complete | 2.663 s | 3.375 s |
| Unfiltered list API calls | 33 | 33 |
| Sum of list API phase durations | 31.454 s | 33.372 s |
| Sum of waits before list attempts | 2.705 ms | 7.299 ms |

All 33 list responses per process were HTTP 200, used reused connections and succeeded on the first attempt. Process B had completed all 33 list calls after HEAD, with **zero media requests**. Subsequent GETs added no list calls. Median request-write to first-response-byte for those list requests was approximately 0.79–0.83 s. Read/decode/processing time accounts for part of the remaining API duration; it must not all be attributed to CPU or all to the network.

The initialization window contained one root metadata call and two filtered path lookups. The test did not observe shortcut-target metadata GETs, About/format discovery, or OAuth refresh in these windows. `env_auth` was false. New connection setup and pacing do not explain the tens-of-seconds interval here. Every GET returned exactly 8388608 bytes. Next-chunk prefetch requests must not be counted as separate client GETs.

These results establish expensive full VFS listings before media for this case. **They do not establish 33 directories, nor 33 pages of one directory.** The revised diagnostic now emits a generated group for each list invocation with its successful-page count and parent-argument count, so the next test can distinguish pagination from multiple lookups without disclosing identifiers.

Limits: one object, two complete processes, live network/background workloads, one cold observation per process. Earlier interrupted starts were excluded. No general speedup percentage is inferred from these measurements.

## Existing capability and chosen scope

Drive already supports a point lookup through `NewObject`: resolve directory IDs using the existing backend cache, then issue a name-filtered query within the resolved parent. `Object.Open` then uses its provider ID. No new public file-ID endpoint is needed for this path. An externally indexed parent could also be configured through the existing root-folder-ID mechanism outside the repository; this change does not require that configuration change.

The existing authenticated `--rc-serve` file route already uses `NewObject` without VFS directory listing. A synthetic test exercises that actual route: HEAD requires one filtered query plus one extra metadata GET from its HTTP object helper, no unfiltered list and no media. Its GET supports ranges. This route has a different server/API surface and error/HTTP behavior and is not deployed as a replacement for `serve http`.

The new `serve http --direct-read` option instead keeps the existing HTTP endpoint and authentication middleware. It is off by default. For a known-size file, it calls the same backend `NewObject` directly and serves the result through Go's existing `http.ServeContent`. The ordinary VFS lookup and all default behavior remain unchanged when the option is absent. No generic VFS Stat semantics, feature interfaces, or public ID APIs change.

## Enabling the experimental mode

In a separately authorized isolated test, add:

```text
--direct-read --read-only --vfs-cache-mode off --drive-read-timing
```

Keep the prior endpoint, remote root, authentication, route and read chunk setting. Do not remove a security-related option to make this mode start. It intentionally rejects active rclone filters, auth proxy, VFS link/metadata-file translation, case-insensitive lookup, normalized duplicate blocking, no-seek and no-modtime. These combinations retain their VFS implementation; they are not silently approximated by point lookup. An active filter appearing in the server context after initialization causes the file path to fall back to VFS.

Known file paths use the backend's exact name and duplicate-selection semantics. Directory URLs still use VFS. Unknown-size objects also fall back to VFS. This is a known-file read mode, not an optimized directory browser. The existing favicon/directory UI paths can still cause VFS access. It should be tested with a controlled HEAD/GET client before use in a browser-facing application.

File reads use a small lazy seeker. `http.ServeContent` can find the size, validate ranges, return conditional responses, and seek before the first backend open. Each actual backend open is bounded by the configured VFS read chunk size (when enabled). The existing reopen-on-error reader handles retries within that range. The existing accounting seeker handles byte accounting, transfer limits and bandwidth limiting. No async VFS buffer, disk file cache, or parallel prefetch is used in this file path. Thus `--buffer-size` and VFS read-ahead/parallel-stream settings do not create a buffer for direct files. This difference must be recorded when comparing modes.

HEAD resolves the object and sets headers, with no media open. GET, suffix ranges, multipart byte ranges and conditional GET remain on `http.ServeContent`. Requests use the original request context; cancellation stops further opens and closes active bodies. Invalid path components, parent traversal and platform volume paths are rejected before backend lookup. Basic/TLS HTTP authentication remains ahead of lookup, without adding an RC administrative endpoint.

## Identity, visibility and invalidation

There is deliberately **no new file-object cache**. Every file request resolves its path again. Warm direct requests therefore make a new name-filtered lookup, whereas a warm VFS request can reuse its directory entry without metadata traffic. This trades one round trip per warm file request for avoiding cold enumeration of unrelated entries. Measure both cold and warm behavior.

A renamed, moved or deleted file is looked up by its current path on the next request; an old path returns not found rather than reusing a stale stored file ID. In-flight reads keep the object identity they opened. The backend's existing directory-ID cache and its native invalidation semantics remain in effect. Moving a cached ancestor directory is not given a new stronger invalidation guarantee by this option.

Drive point lookup also now applies `--drive-auth-owner-only` to candidate file entries, matching the restriction already applied by directory listing. A regression test first demonstrated that direct lookup otherwise returned an unowned candidate despite that flag; owned and unowned synthetic cases verify the correction. The default false value is unchanged.

The implementation reuses Drive's own name lookup, shortcut resolution, resource-key handling and shared-drive query parameters. Synthetic HTTP tests include a resource-key-protected object and shared-drive list parameters. It does not invent a provider-ID namespace or bypass a configured served root by accepting arbitrary IDs. A future public ID endpoint would separately need served-root membership validation in addition to provider permission checks.

Duplicate names retain native backend point-lookup semantics; callers needing unique stable identity should not assume that a name identifies one provider object. Dangling shortcuts and Google document export paths remain backend responsibilities. Real-cloud edge-case coverage for moved ancestors, duplicate selection and document exports is not claimed here.

## Reproduction and API-call results

`TestDriveHTTPDirectRead` starts the actual HTTP server through the existing `serve/start` registry, with a synthetic Drive API and a directory containing 32001 files on 33 pages. The target is on the last page. Both modes use the same fixture, shared-drive constraints, synthetic resource key and bounded 8 MiB media ranges. No real credentials are involved.

| Client operation | Default VFS: unfiltered / filtered / metadata / media | Direct read: unfiltered / filtered / metadata / media |
|---|---:|---:|
| Cold HEAD | 33 / 0 / 0 / 0 | 0 / 1 / 0 / 0 |
| Following 8 MiB GET | 0 / 0 / 0 / 1 | 0 / 1 / 0 / 1 |
| Repeat 8 MiB GET | 0 / 0 / 0 / 1 | 0 / 1 / 0 / 1 |

These are per-client-request counts. The ordinary VFS case documents the existing behavior; it is not asserted to be a bug for directory browsing. The direct-read test initially failed because the option was unsupported, then passes with zero sibling enumeration. Separate pre-fix owner-filter evidence is retained in local validation outputs. The original VFS initial-seek patch remains an earlier separate commit.

Run with a clean environment and dedicated Go 1.26.0 caches as documented in the baseline:

```sh
go test ./backend/drive -run '^TestDriveHTTPDirectRead$' -count=3 -v
go test ./backend/drive -run '^TestDriveDirectLookupOwnerFilter$' -count=1
go test ./cmd/serve/http ./backend/drive ./fs/rc/rcserver -count=1
go test -race ./cmd/serve/http ./backend/drive \
  -run '^(TestDirectRead|TestDriveCold|TestDriveHTTPDirectRead|TestDriveDirectLookupOwnerFilter|TestDriveReadTiming)' -count=1
```

The new HTTP tests also cover auth rejection before lookup, HEAD with no body, suffix and multipart ranges, 416 bounds, conditional 304, traversal rejection, rename/delete visibility without sibling listing, sequential bounded opens, cancellation before and during a read, and unsupported-option rejection. Current real cold timing improvements for this new option remain to be established by the separately authorized repeat test. Production is not changed by this work, and no upstream PR is opened.
