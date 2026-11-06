# Locating cold Drive read latency

## What source inspection establishes

The previously supplied aggregate timings alone did not localize the remaining real cold wait. The subsequent authorized phase test is recorded in [the cold-read findings and opt-in mode](drive-cold-read-fix.md). The seek patch removes an unnecessary media open, but the first request still waits tens of seconds. No additional private files, production logs, servers or real cloud objects were accessed for this investigation.

The first HTTP request can execute this sequence:

1. Resolve VFS directories and populate their in-memory caches.
2. Refresh OAuth credentials if the first provider call needs it. With a known configured root, authentication may remain lazy until that first call. Without a configured root, root discovery normally causes an earlier provider request during backend creation, before the server begins accepting requests.
3. List every page of each cold parent directory, with no name filter in the VFS listing path.
4. Resolve shortcuts encountered in those pages with additional metadata GETs, sequentially. This happens even when reading an unrelated regular video. The target metadata results are not reused between duplicate shortcuts in this path.
5. Fetch Drive export/import formats through `About.Get` once per process if conversion of a checksumless entry requires format discovery. A regular video with an MD5 checksum does not itself require this call, but other entries in a full directory listing can.
6. Build/sort VFS nodes, open the selected object, establish any required media connection, receive its headers and consume the first body data. Chunking and buffer settings can affect when data reach the HTTP client.

There is no fixed 40-second startup sleep in these paths. Drive defaults are a 100-request initial burst and a 100 ms minimum pacing interval after the burst. On retriable errors the Google Drive pacer uses exponential backoff up to 16 seconds plus random jitter; repeated failures can therefore accumulate tens of seconds. The diagnostic must distinguish waiting between attempts from time spent inside a slow API request. It must not assume that backoff actually occurred.

Relevant source: `cmd/serve/http/http.go:serveFile`, `vfs/dir.go:_readDir`, Drive `NewFs`, `list`, `resolveShortcut`, `findExportFormat`, `fetchFormats`, `httpResponse`, and `lib/pacer/pacers.go:GoogleDrive.Calculate`.

## Additional synthetic evidence

`TestDriveColdShortcutAmplification` lists a synthetic directory containing one regular target file and twelve unrelated shortcuts to the same target ID. Each provider request is delayed by 5 ms. The first `VFS.Stat` takes approximately 72 ms and makes **one list plus twelve metadata GETs, with zero media requests**. A second Stat uses the directory cache and takes microseconds, making no new calls. Direct `NewObject` for the regular file does not resolve those unrelated shortcuts.

This establishes an amplification mechanism, not the contents or cause of a real user's directory. No shortcut cache or new lookup semantics are introduced in this diagnostic change.

`TestDriveColdReadAttribution` separately injects approximately 120 ms into OAuth, two list pages, or media response headers. All three can make the first read similarly slow. The timing records distinguish them:

| Injected phase | First Stat | First byte of first read | First byte of warm read |
|---|---:|---:|---:|
| OAuth, 120 ms once | 131.75 ms | 131.91 ms | 0.18 ms |
| Listing, two 60 ms pages | 143.13 ms | 143.66 ms | 0.53 ms |
| Media headers, 120 ms per request | 18.76 ms | 139.74 ms | 121.04 ms |

These are illustrative single loopback runs with generated data, not production benchmarks. They show why cold and warm total times alone do not establish a cause.

## Opt-in instrumentation

Add `--drive-read-timing` to a separately authorized isolated test invocation, retaining its existing network and VFS settings. It is false by default. It emits NOTICE records prefixed with `DRIVE READ TIMING`. No request URLs, queries, paths, provider IDs, credentials, header values, error messages, response contents, IP addresses or hostnames are included in those records. The `id` field is a generated process-local event-group counter, not a provider ID.

The instrumentation records:

- `backend-created` and `root-path-finished`: construction versus root/path setup.
- API `phase-start`, `attempt-start`, `attempt-finished`, `phase-finished`: waiting for the pacer/connection limit separately from attempts, with retry and failure booleans.
- `http-start`, DNS/TCP/TLS events, `connection-ready reused=...`, `request-written`, `first-response-byte`, `http-headers status=...`: per HTTP round trip.
- `body-first-read`, `body-read-finished`, `body-closed`: first consumed body data, end/early close, and actual bytes read.

A media API phase wraps `client.Do`, so it includes any token refresh and redirects. Individual transport requests are logged separately; OAuth can be separated from the Drive request. API list attempt time includes response decoding. Body-close time also includes time the consumer spent consuming/processing the body. Parallel connection callbacks can overlap, so do not sum DNS/TCP/TLS timestamps as independent sequential spans. Do not add parent API durations to their nested HTTP/OAuth durations.

Transport timing covers the configured user OAuth and service-account clients. With `env_auth`, the external client is left unchanged; only API phase timing is available. The optional v2 media-metadata call has transport events but no separate API pacing span. Existing HTTP trace hooks are composed rather than replaced. Other background requests in the same process also appear; run one controlled client and retain a bounded measurement window.

Use NOTICE level, not request/header/body dump flags. Ordinary rclone log records may still contain private data, irrespective of this flag. Keep the complete log private and send only the analyzer's numeric output.

## Smallest next real-world experiment

This section is a proposed experiment, not authorization to access a server or archive. The original separate-authorization boundary remains in force. Production should continue using the official binary.

Use the same isolated read-only setup as the previous build comparison. Do not introduce a new route, token, buffer size, chunk size or listing option merely to improve a measurement. Retain five-minute directory caching, disk cache off, 1 MiB buffer and 8 MiB chunks if reproducing that comparison. Capture startup records as well as the first client request.

Run two fresh isolated processes, for the same separately authorized regular object:

- A: first request is the 8 MiB ranged GET, then repeat it.
- B: first request is HEAD, followed by the same ranged GET.

In `serve http`, HEAD executes VFS lookup and metadata/header preparation, then returns before `file.Open`. If the 40 seconds move into HEAD, the wait is before opening media. If HEAD is fast but the following GET is slow, inspect media connection/header/first-body timings. This probe does not by itself distinguish OAuth from listing; the phase records provide that distinction.

Keep URLs private. For example, with `PRIVATE_URL` already supplied outside the repository, these commands suppress response headers and bodies and print only status/timing/size values:

```sh
curl --silent --show-error --head --output /dev/null \
  --write-out 'HEAD status=%{http_code} headers=%{time_starttransfer} total=%{time_total}\n' \
  "$PRIVATE_URL"

curl --silent --show-error --header 'Range: bytes=8388608-16777215' --output /dev/null \
  --write-out 'GET status=%{http_code} headers=%{time_starttransfer} total=%{time_total} bytes=%{size_download}\n' \
  "$PRIVATE_URL"
```

The intended GET response is 206 with exactly 8388608 bytes. Do not disable TLS verification or authentication. Keep any curl errors private too, since external tools can include URLs in their errors. HEAD does not download the media body for this regular known-size file path, but it can read remote directory/metadata information and requires the same authorization.

## Analyzer and interpretation

Run on private logs outside the checkout:

```sh
python3 bin/drive_read_timing.py < "$PRIVATE_TIMING_LOG" > "$PRIVATE_TIMING_SUMMARY"
```

The analyzer reads stdin, accepts only the timing schema and known operation/event labels, ignores unrelated log text, and emits generated group numbers, booleans, status codes, byte counts and timings. Use records from exactly one process. It rejects repeated start IDs rather than combining restarted process logs. Its operation totals cover the supplied window and may include background requests. The analyzer does not copy private raw logs into this repository.

Look for:

| Observation | Localization |
|---|---|
| Slow OAuth HTTP request | Credential refresh HTTP path; use connection/request-write events to separate connection setup from response wait |
| Many slow `list-directory` requests | Parent directory pagination before media |
| Many `metadata` calls between listing and media | Extra resolution, including shortcuts; a plain regular-file listing otherwise provides object metadata |
| Slow `about` / `formats` | Lazy format discovery while processing other entries |
| Large `pacer_wait_ms`, repeated attempts, retry=true | Pacing, connection limiting or retry backoff, rather than transfer time |
| Slow connection-ready or TLS events | DNS, dial/proxy/TLS path, before provider response |
| Fast connection/write but slow HTTP headers | Waiting after request write; includes network/provider response latency, not proof of a specific server cause |
| Media headers fast, first body read slow | Media body availability before rclone consumes data |
| All provider phases fast, client headers slow | Investigate VFS processing/buffering/scheduling and correlate the client window; do not attribute the gap to Google API without evidence |

This protocol subsequently obtained an authorized real phase summary: the first HEAD spent approximately 33 seconds in VFS preparation, including 33 unfiltered list API calls, before any media request. See [the findings and opt-in mode](drive-cold-read-fix.md). The original aggregate 38–52 second totals alone did not contain that phase information. New listing-start/listing-finished groups record the number of pages per list invocation, without provider identifiers; do not assume the observed 33 calls were pages of one folder.

## Validation and limits

The source change is diagnostic and opt-in. It does not attempt a speculative cold-read optimization. Tests cover TLS and connection reuse, logging without synthetic secrets/URLs/IDs/header/body values, transport errors, cancellation, retry pacing, cold attribution, shortcut amplification, and unchanged default read behavior. The analyzer was checked against synthetic OAuth/list/media counts and injected pacing, ignored unrelated text and duplicate process logs. A full static linux/amd64 CLI build and targeted race verification are included in local validation. Cloud integration and the repository-wide quicktest are not claimed.
