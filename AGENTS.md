# Coding-agent guidance

## Orientation

- `cmd/dublift/`: server CLI; flags select the private config and listen address.
- `cmd/dublift-check/`: optional live provider checker.
- `internal/dublift/`: addon discovery, HTTP/range handling, indexing, media
  processing, audio alignment, sessions, and persistence. Tests live beside
  the code they exercise.
- `internal/dublift/web/`: embedded dashboard HTML, CSS, JavaScript, and icon.
  Rebuild the Go binary to serve asset changes.
- `scripts/` and `Makefile`: local run, validation, and Linux release builds.

## Invariants

- Stream listing fetches upstream results and checks Vixsrc Italian audio.
  Defer source probing, indexing, and decoding until preparation or playback.
- Preserve upstream ordering and complete fallback stream objects, including
  unknown JSON fields. The explicit donation-link exclusion in `discovery.go`
  is intentional.
- Transformed playback uses local playlists and media endpoints. Preserve
  origin headers/cookies, HLS resource URLs, byte ranges, and media clocks.
- Stream-copy source video. Keep origin reads, caches, extraction windows,
  and subprocess output bounded; do not add whole-file download fallbacks.
- Preserve foreground playback priority over background alignment. Context
  cancellation must stop obsolete reads and jobs; canceled work must not
  repopulate discarded cache/session state.
- HEAD probes must not select playback. Result refreshes preserve active
  playback; selecting another stream replaces it. Dashboard polling must
  not extend media-session lifetimes.
- Positive offsets delay Vixsrc audio. Keep manual timing, confidence
  fallback, saved boundaries, and source identity consistent with subtitles.
- Keep preparation, alignment download, and calculation deadlines separate
  (`timeouts.go`). Waiting for a playback window must not consume the latter
  two deadlines.
- Settings/mutation APIs stay same-origin; addon/media endpoints need player
  CORS. Keep signed URLs, addon credentials, config, reports, and playback
  keys out of tracked files and public diagnostics.
- Android-specific FFmpeg options, system fonts, and OpenH264 support serve
  [DubLiftApp](https://github.com/joojoooo/DubLiftApp), which builds this server
  from source. Preserve that integration; Linux release scripts do not build
  standalone Android binaries.

## Changes and validation

- Use `gofmt` and the existing Go/package conventions. The dashboard uses
  plain JavaScript and embedded assets; no Node runtime is needed to run it.
- Settings changes must agree across `config.go`, dashboard fields in
  `web/index.html` and `web/app.js`, and relevant validation/tests. JSON API
  changes must agree with dashboard consumers and endpoint tests.
- Keep `scripts/build.sh`, `Makefile` cleanup targets, and documented release
  platforms aligned. Match the documented Go requirement to `go.mod`.
- Use local mock servers and synthetic media for regression tests. Keep
  third-party availability out of the ordinary test suite.
- Follow [the validation reference](docs/validation.md) for commands and
  prerequisites. Run checks relevant to the change and report skipped checks.
- Keep `README.md` concise and user-focused; put operational details in
  `docs/usage.md`. Keep canonical validation guidance in `docs/validation.md`.
  Update affected docs when commands, settings, or supported behavior change.
- Store only durable agent guidance here.