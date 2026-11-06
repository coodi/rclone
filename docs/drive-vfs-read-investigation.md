# Drive VFS first-read and seek investigation

This follows the backend-only [initial baseline](drive-read-baseline.md). That baseline did not exercise `serve http`'s VFS lookup and must not be used to rule out full directory listing in `serve http`.

## Verified scope

The official `v1.75.2` tag resolves to commit `be8d6044e` (annotated tag object `b0dd53938615bb610c64f610eb2b17be16752f77`). A detached scratch worktree was tested before and after applying only the VFS seek patch. Current upstream `7c60060d4cb811fba5cf4e814207df4c7091d4df` was also tested. Both versions require Go 1.26.0 and were tested with that toolchain. No real cloud, production server, private archive or configuration was accessed.

The reported real first-header delay of approximately 36 seconds has **not** been reproduced or attributed. The deterministic local test establishes API-call behavior and one separate, fixable seek inefficiency. It does not establish the cause or duration of a production wait.

## Why the first HTTP file request can list a whole folder

`cmd/serve/http/http.go:serveFile` calls `VFS.Stat`, then opens the VFS file and serves it through `http.ServeContent`. On a cold directory, `vfs/dir.go:_readDir` calls `list.DirSorted`. Drive `List`/`ListP` calls its list helper with an empty title: the provider query contains the parent and trash constraints but **no file-name filter**. It consumes every result page before resolving the VFS node.

This differs from a direct Drive `NewObject(path)`, which uses the directory-ID cache and a name-filtered query for the file. Both paths existed in 1.75.2 and current upstream. Turning off the disk VFS cache leaves the directory lookup and in-memory directory cache in use. With a five-minute directory cache, a warm request can reuse the VFS object without either a list or a metadata GET until cache expiry or invalidation. Intermediate directories can require their own listings as the VFS walks a nested path.

A full first listing is therefore verified behavior. Its responsibility for the reported production delay is still a hypothesis: token refresh, provider/network latency, pacing/retries, directory pagination and client initialization remain possible contributors.

## Controlled synthetic test

`TestDriveVFSFirstRange` in `backend/drive/read_latency_internal_test.go` uses the real Drive backend, VFS `Stat`, file `Open` and Go `http.ServeContent`. Its small loopback HTTP handler mirrors those relevant calls from `serve http`; it does **not** instantiate the entire command server, authentication middleware or CLI process. The existing `cmd/serve/http` package tests are also run against the patch.

The fake provider has 1, 1001 or 10001 files, pages of 1000 entries, a known root ID, and the target file on the last page. It honors the difference between filtered and unfiltered lookups. The synthetic object is 1 GiB. Clients request three 8 MiB ranges: offset zero, offset 8 MiB, then zero again. Responses must be HTTP 206 with the exact requested body length; SHA-256 of the repeated first range must match. Data are generated fixtures, not real file contents.

Settings: disk VFS cache off, directory cache five minutes, buffer size zero, read chunks fixed at 8 MiB, no polling, no Google documents or v2 download path. Buffer/chunk settings are controlled test settings, not assumed production settings. The existing directory-depth benchmark separately exercises 0/4/16 parents and direct lookup versus known IDs.

The fake provider injects 2 ms per HTTP request, including a loopback OAuth refresh endpoint. An expired dummy token forces one real `oauth2` refresh through that endpoint. Tokens and client identifiers are synthetic constants and never go to Google. The normal Drive pacer is configured with a large burst and zero minimum delay to isolate request count; provider throttling is not simulated.

Recorded phases:

- OAuth: exactly one refresh on the first provider request; no further refresh for the subsequent ranges. Logs report its handler duration separately.
- Root and path: the HTTP matrix uses an already known root, with zero root-resolution API calls. `TestDriveRootIDBaseline` separately exercises `getRootID`: one refresh and one root metadata GET, no list. Nested path resolution is covered by the earlier depth benchmark; full `NewFs` construction is not timed.
- Directory: unfiltered list pages and cumulative list-handler time, including the injected delay and response generation.
- Metadata: no per-file metadata GET for the VFS requests, because list results already contain the needed object fields. A separate known-ID read verifies one metadata GET without directory listing.
- Media: request count, actual written media bytes, client header latency and full body completion time. Body completion includes client SHA-256 work. Handler phase times are cumulative server spans; they do not add up exactly to client elapsed time because HTTP transport, decoding and VFS processing are outside them.

The test does not reproduce actual OAuth service latency, DNS/TLS, Google throttling, HTTP/2, bandwidth limits, or a full process startup. It does not attempt to synthesize an unexplained 36-second pause.

## Reproduced seek inefficiency and minimal patch

Before the patch, `vfs.ReadFileHandle.readAt` calls `openPending` at offset zero even when the first read already specifies a different offset. It then seeks by reopening the backend at the requested offset. The new generic VFS regression test fails on unmodified current upstream with backend open offsets `[0, 5]` instead of `[5]`, for both `Seek` followed by `Read` and initial `ReadAt`.

The patch changes only pending-open initialization: for a valid, nonzero initial offset on a seekable handle, position the existing chunked reader before opening it. After success, update its tracked offset and disable the whole-file checksum, as an ordinary seek already does. Zero-offset reads, unknown-size files, no-seek handles and out-of-bounds offsets follow the existing path. No flags, public APIs, authentication behavior, global caches or backend-specific special cases are added. No new buffering is introduced. Sequential and parallel chunk readers are covered by the regression test.

This removes one unnecessary media request on the first nonzero read of each fresh VFS read handle. It does **not** reduce the first directory listing or the first read at offset zero.

## Before/after measurements

Three runs per case on the same 1.75.2 scratch worktree, first unmodified and then with only the seek patch. No compilation or verification workload ran concurrently with these final measurements. linux/amd64, Intel Core i7-13700, Go 1.26.0. The table shows medians for the directory containing 10001 files. Counts below are per HTTP client range, not cumulative.

| Client range | List calls, before / after | Media calls, before / after | Headers ms, before / after | Complete 8 MiB ms, before / after |
|---|---:|---:|---:|---:|
| First range, offset 0 | 11 / 11 | 1 / 1 | 166.34 / 151.22 | 183.73 / 173.69 |
| Next range, offset 8 MiB | 0 / 0 | 2 / 1 | 4.89 / 2.44 | 12.24 / 9.46 |
| Repeat first range | 0 / 0 | 1 / 1 | 2.53 / 2.43 | 18.33 / 16.54 |

The changed request count is the robust result. The variation of the cold zero-offset time is unrelated to the patch: that request takes the same code path and still lists eleven pages. These local millisecond timings must not be extrapolated into a production speedup. The wasted response before a seek may transfer a variable number of bytes before it is closed, depending on network buffering.

## Reading an externally indexed Google file ID

The existing internal route is `getFile(id, fields)` followed by `newObjectWithInfo`, then `Object.Open` with a `RangeOption`. The test exercises an 8 MiB read through it: one metadata GET, one media request and no list calls. Retaining that object for subsequent ranges removes the repeated metadata request. A metadata snapshot from an external index still needs suitable validation and invalidation; a raw ID alone does not encode size, MIME type, resource keys or shortcut semantics.

There is no existing general public ranged-read-by-ID endpoint in the paths inspected. Drive `backend copyid` already resolves and copies a file by ID; it is not such an endpoint. Introducing an opt-in Drive read-by-ID API is a separate follow-up, requiring authorization checks, resource-key/shortcut behavior, missing/moved IDs, shared-drive tests, bounded streaming and cancellation semantics. This patch does not invent or expose that API.

## Reproduction and validation

Use the clean environment and dedicated caches described in the initial baseline. On unmodified source, copy in the synthetic test file and run:

```sh
"$GO_BIN" test ./backend/drive -run '^(TestDriveVFSFirstRange|TestDriveRootIDBaseline)$' -v -count=3
```

Apply the `vfs/read.go` change alone and repeat the same command. To reproduce the regression failure on unmodified current upstream, also add the `vfs/read_test.go` regression test and run `go test ./vfs -run '^TestReadFileHandleInitialOffset$' -count=1`; then apply the production change and repeat.

Validation on the current fork: full CLI build; complete local package tests for `vfs`, `backend/drive`, `cmd/serve/http`, `fs/chunkedreader`; targeted race tests for read handles and the synthetic Drive scenarios. Integration runs against real cloud backends and the whole repository's `make quicktest` are not claimed. MIT licensing and module versions remain unchanged. No upstream PR was opened.

The next first-byte investigation should instrument the full HTTP command's `NewFs`, OAuth transport, directory listing pages and media connection separately in a synthetic environment. A public ID API or a change to VFS directory lookup is not justified as a proven fix for the reported first-read delay by these results alone.
