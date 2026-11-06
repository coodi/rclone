# Drive read latency investigation

This is an experimental open-source fork of [rclone](https://github.com/rclone/rclone). The upstream MIT license, copyright notices and contribution instructions remain in effect. No performance patch has been implemented yet.

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
