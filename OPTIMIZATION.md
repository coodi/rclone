# Drive read latency investigation

This is an experimental open-source fork of [rclone](https://github.com/rclone/rclone). The upstream MIT license, copyright notices and contribution instructions remain in effect. A minimal VFS initial-seek patch is under local test; no fix for the reported cold first-read delay is claimed.

## Objective

Reduce time to first byte and repeated seek overhead for original media stored in Google Drive. Investigate addressing an already indexed object by its stable provider ID, reuse of backend instances and connections, and bounded streaming without downloading an entire file first. Preserve existing CLI, RC and backend behavior.

## Starting evidence and limits

A downstream deployment using Debian's rclone 1.60.1 observed approximately 38 seconds for an 8 MiB range in a new CLI process. A private persistent HTTP reader reduced subsequent ranges to approximately 3–5 seconds, while the first uncached request still took about 40 seconds. Direct provider API reads were faster in that environment. These observations are not controlled upstream benchmarks, do not establish the cause, and must not be advertised as a speedup of this fork. Network route, DNS, provider pacing, authentication, directory contents, and process lifecycle are confounding variables.

## First investigation

1. Build current upstream and record its revision, Go version and dependencies. Compare it with the older release before changing source.
2. Inspect existing Drive object-ID, directory-cache, backend-command and RC functionality. Reuse existing capabilities; do not assume direct-ID reading needs a new API.
3. Separate process startup, OAuth refresh, root resolution, directory listing, object metadata, first byte and body transfer. Compare cold and warm runs, repeated seeks and concurrent transfers.
4. Build a deterministic fake Google API benchmark with synthetic directories and files. Count API calls and bytes, not only elapsed time. Use several directory sizes, duplicate names, moved/deleted IDs, shortcuts and shared drives where relevant.
5. Add a failing regression test before the smallest compatible fix. Run targeted tests, race checks where appropriate, and bounded-memory/cancellation tests. Publish reproducible benchmark commands and limitations.

## Privacy and operational boundaries

The public repository must contain only upstream source, generic documentation, synthetic fixtures and reviewed changes. Never import a downstream application's code, database, logs, remote configuration, cookies, OAuth tokens, provider file IDs, personal filenames, account identifiers, server addresses or SSH keys. Do not enable HTTP header/auth dumps in public artifacts.

Cloud integration tests are opt-in and use credentials outside the checkout. Never read a real user archive or change a running production deployment without explicit authorization for that work. Use read-only measurements; do not move, delete, overwrite or transcode source files. Report when tests cannot run, and do not describe a hypothesis as a measured result.

Follow AGENTS.md and CONTRIBUTING.md. Preserve attribution and do not submit an upstream pull request without maintainer review of the actual code and results.

## Baseline: 2026-11-06

The first synthetic investigation is documented in [docs/drive-read-baseline.md](docs/drive-read-baseline.md). The benchmark exercises unchanged upstream production code at `7c60060d4cb811fba5cf4e814207df4c7091d4df`; it adds test scaffolding only. It does not reproduce or explain the historical 38-second observation. No performance fix is claimed.

## VFS and 1.75.2 follow-up

[docs/drive-vfs-read-investigation.md](docs/drive-vfs-read-investigation.md) records the full-listing path used by `serve http`, 8 MiB synthetic HTTP ranges on official 1.75.2, and a generic VFS patch removing an unnecessary zero-offset media open before an initial nonzero read. This seek patch does not change directory listing or explain the reported cold first-byte delay.

## Cold-read attribution

[docs/drive-cold-read-diagnostics.md](docs/drive-cold-read-diagnostics.md) records additional pre-media work (including sequential resolution of unrelated shortcuts), synthetic phase attribution, and opt-in `--drive-read-timing` instrumentation. The next discriminating authorized experiment is a cold HEAD followed by GET, with anonymous phase timing records. A real cold-delay cause is not claimed from aggregate timings alone.

## Localized cold lookup and opt-in HTTP mode

[docs/drive-cold-read-fix.md](docs/drive-cold-read-fix.md) records the authorized aggregate HEAD/GET phase results: approximately 33 seconds before media, dominated by 33 unfiltered list calls. The new off-by-default `serve http --direct-read` path reuses backend point lookup instead of sibling enumeration for known-size files, with explicit configuration guards and per-request path revalidation. Synthetic HTTP tests compare 33 full-list calls with one filtered lookup. Real validation of this new mode is separate from the earlier seek-patch measurements.

## Stable-base backport

Branch `codex/v1.75.2-direct-read` starts at the official stable tag `v1.75.2`. It contains the isolated initial-seek fix, opt-in read diagnostics, opt-in direct HTTP reading, the matching Drive owner restriction fix and their tests/docs. Unrelated 1.76 development changes are excluded. `--direct-read` remains off by default, requires read-only/cache-off operation, and preserves its unsupported-option guards. This is a modified stable-base fork, not an official upstream release. Deployment validation and private real-object measurements are performed outside this public repository; no private configuration or trace is imported here.
