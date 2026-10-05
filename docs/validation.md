# Validation

Run commands from the repository root. Use the Go version required by
`go.mod` (currently 1.26.2 or newer). Install FFmpeg and ffprobe for media
integration tests; `-race` also requires a working C toolchain.

## Canonical checks

```sh
go test -race ./...
go vet ./...
go mod verify
test -z "$(gofmt -l cmd internal)"
git diff --check
make build
make release
```

- **Tests:** local mock servers and synthetic media check discovery,
  config version resets, source toggles/order, upstream preservation, VixSrc
  qualities and native playback across restarts, HTTP ranges and retries,
  HLS proxying, file remuxing/timing, generated audio, alignment, dashboard APIs, and
  session/cache lifecycle. The race detector checks executed Go code for data
  races.
- **Vet / module verification:** static Go diagnostics and cached dependency
  integrity. These do not prove playback or provider availability.
- **Formatting / diff:** Go formatting and patch whitespace. Review Markdown
  links and commands when changing docs; there is no checked-in docs linter.
- **Builds:** `make build` compiles the local server; `make release` uses
  `scripts/build.sh` to build Linux amd64/arm64 binaries with CGO disabled.
  Cross-compilation does not prove execution on the target hardware.

Media tests skip when FFmpeg/ffprobe are missing. Fixtures also need the
encoders used by the tests, including `libx264` and AAC; HEVC/AV1 tests skip
without `libx265`/`libsvtav1`. Error-clip tests need `lavfi`, `drawtext`, and a
usable font. Inspect skip messages before claiming media coverage.

## Dashboard sources, filters, and titles

For dashboard source-editing, filtering, or title-display changes, also run the
regressions with Node.js (no third-party packages are needed):

```sh
node --test scripts/dashboard-sources.test.cjs
node --test scripts/dashboard-filters.test.cjs
node --test scripts/dashboard-content.test.cjs
```

These check valid addon additions, rapid reorder saves, draft URLs, failed
saves, reuse of addon details on blur and reorder, lookup retries, source editing
waiting for synchronization between settings and setup,
manifest validation during pending saves, and preserving other settings.
They use a small DOM fixture and do not check browser rendering. Filter checks
cover combined source/type selections, saved
preferences, source refreshes, result counts, and keeping playback visible.
Title checks cover resolver presets, consistent headings, decorated title
deduplication, and preserving technical stream details.
Node.js is not needed to run the server.

## Optional playback and provider checks

With VLC installed, run:

```sh
DUBLIFT_VLC_TESTS=1 go test ./internal/dublift -run '^TestVirtualHLSEndToEnd$' -count=1 -v
```

This uses synthetic MKV, MPEG-TS HLS, and fMP4 HLS sources. It requires VLC
to decode video and select Italian audio both before and after a seek.

With upstream addons configured in a private config, run:

```sh
go run ./cmd/dublift-check -config .local/config.json -out .local/live-report.json
```

The checker fetches stream lists and Vixsrc manifests for its built-in title
set (S1E1 for series). It reports availability and errors; it does not assert
success, decode playback, or download complete titles. Keep reports private.

## Manual checks and gaps

- Start `./bin/dublift` with a private test config. Check `/healthz`, guided
  setup, settings save/reload, source toggles/reordering/removal, VixSrc-only setup,
  and manifest installation.
- Check desktop and narrow mobile dashboard layouts, browser errors,
  stream ordering/filtering, preparation, and manual timing/reset controls.
- Play through Stremio/Nuvio and VLC: verify video and Italian audio at
  startup, after forward/backward seeks, and after timing changes. Refresh
  results during playback and select a different stream to check lifecycle.
- Test from another LAN device with a reachable Public base URL. Check real
  codecs/HDR, long playback, and slow or failing providers on target players.

Automated tests do not cover browser rendering, physical Stremio/Nuvio
players, full-title playback, or third-party uptime. There is no CI workflow
in this repository. Android APK and device validation belongs to
[DubLiftApp](https://github.com/joojoooo/DubLiftApp).
