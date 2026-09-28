# DubLift

A local Stremio/Nuvio HTTP addon that combines an upstream stream's video with Vixsrc Italian and English audio. Go serves the addon, dashboard, header proxy, alignment engine and media endpoints. FFmpeg handles bounded audio extraction, AAC encoding and video **stream copy**. The supplied Vixsrc scraper’s resolver is ported to Go; no JavaScript runtime is needed.

## Start on Linux

Install Go **1.26.2 or newer**, FFmpeg and ffprobe, then:

```sh
go build -trimpath -o bin/dublift ./cmd/dublift
./bin/dublift
```

Open **http://localhost:7000**. Add your upstream addon manifest URLs in Source settings. Install `http://YOUR-LAN-IP:7000/manifest.json` in Stremio or Nuvio. VLC can open a session's HLS URL from the dashboard. Use the LAN address on other devices; `localhost` always means the device running the player.

Settings default to `.local/config.json`, with mode `0600`. The local configuration directory, alignment database, generated binaries, development reports and credentials are ignored by Git. Fresh installations have no upstream URLs. This workspace's supplied upstream was placed only in its private local configuration.

```sh
./bin/dublift -config /path/to/private/config.json
./bin/dublift -listen 127.0.0.1:7000
```

The default bind is `0.0.0.0:7000`. The dashboard is intended for a trusted local network. Its settings API rejects cross-origin browser requests; addon and media endpoints provide normal Stremio CORS. There is no built-in remote-access authentication.

## Direct playback and stopping DubLift

**Prefer proxying upstream media through DubLift** defaults off. Accessible HLS video points directly to its origin. Sources that need headers/cookies, and file sources needing remuxing, retain the required local endpoints.

**Use direct Vixsrc audio when possible** defaults on, with a **±0.125 second** tolerance. Every configured sample in the latest automatic alignment run must succeed above the confidence threshold, cover early and later playback, agree, and remain within the chosen tolerance. At least two samples are required to check the runtime. Native media clocks and discontinuities also have to fit. Failed samples, saved timeline corrections, or a larger manual override keep generated audio active.

Choose **Infinite · synchronize in the player** to bypass alignment and confidence requirements for direct audio. DubLift still calculates alignment in the background and displays a suggested player delay, including any measured native timestamp difference. Set the delay in your player; subtitle delay may need to be set separately. Infinite tolerance does **not** bypass required headers, unsupported muxed renditions, or file remuxing. Enabling proxy preference takes priority over both finite and infinite direct modes.

Direct access is checked without the scraper's headers or cookies, using bounded requests spread across the playlist. A local VOD playlist snapshot is used when only the upstream playlist needs headers; its segment, key and initialization URLs are absolute origin URLs. No per-segment redirect through DubLift remains in a direct rendition.

The dashboard says when **video and Italian audio can continue directly**. Wait until the player has loaded their playlists before stopping DubLift. Other tracks or generated subtitles may still need it. Playback, seeking and already accessible direct tracks then use the origins; expiring URLs and origin availability still apply. Opening a new title needs DubLift again.

**Start immediately with offset 0** defaults on. Once essential source probing finishes, playback begins while alignment and anonymous-access checks continue in the background. Upcoming generated audio segments adopt the offset after analysis finishes. With this option off, only the first near-start sample gates playback; later samples continue in the background. A failed first attempt releases playback with offset zero. Players generally keep their loaded VOD playlists: if a stream becomes eligible later, or settings change during playback, **reopen its HLS URL** to load the direct paths. The dashboard distinguishes available direct playback from the delivery mode last loaded by the player.

## Synchronization

- English-to-English matching uses `gochromaprint`, then band-energy correlation and PCM refinement. It starts with a small search region and expands up to the configured radius when needed.
- **Alignment samples** is configurable from 1 to 12 (default 3), spread from near the beginning through later playback. The search radius defaults to **8 seconds**, using **16-second reference windows** and a 30-second limit per sample. Existing custom radii are preserved. Consistent anchors produce one offset. When editions differ, playback uses the reliable match nearest the beginning.
- Confidence below the configured threshold, or a missing English reference, produces offset-zero fallback. Streams without usable Italian audio are returned exactly as received from the upstream addon.
- Positive offsets delay Vixsrc relative to the source. Manual offset overrides are applied to upcoming generated audio/subtitle segments; seek or reopen to flush audio already buffered by the player.
- **Realign from current position** uses the most recently requested local media segment, including seeks. A direct player makes no such requests, so the dashboard also accepts the player's position in seconds.
- Accepted realignments become stored timeline boundaries. Sections remain at normal speed; video is never time-stretched.
- Alignment and boundaries persist next to the config in `alignment.json`, keyed by content and source identity. Exact signed URLs and container/playlist identity avoid accidentally reusing another edition's sync; changed signed URLs can require a fresh analysis.

## Stream results and automatic cleanup

Stream discovery always confirms that Vixsrc has Italian audio. By default it then checks source manifests/range indexes and small prefixes of the first HLS video segment and initialization map, without decoding source audio. It checks streams in upstream order until it finds the configured maximum of Italian results (default 3) or reaches the end. Each source check has a configurable timeout (default 25 seconds), and up to the configured number of checks run in parallel (default 1). Earlier streams keep priority when the result limit is reached. Later entries keep their original links. With **Bypass source checks when listing** enabled, all HTTP streams get Italian result links once Vixsrc Italian audio is confirmed; each source is checked when selected for playback or when **Check & prepare** is clicked on the dashboard. The result limit and parallel setting do not apply in this mode. Eligible entries retain the upstream name, title, description and provider details, with only a 🇮🇹 prefix added to the name. Torrents, external players and streams without Vixsrc Italian audio retain their original objects and links in either mode. In normal listing mode, unavailable video resources and non-seekable files also retain their original links. In bypass mode, those source failures are discovered when the stream is prepared. The exact external URL `https://pengu.uk/donate` is omitted from both stream results and the dashboard. If video probing fails later during preparation, the selected master redirects to its original link; DubLift does not publish an audio-only replacement.

The dashboard preserves the exact response order: configured-addon order, then each addon's original stream order. It displays full stream text and resolves the movie/series name through addon metadata, Cinemeta for IMDb IDs, or an optional TMDB token. When metadata is unavailable, the upstream title/description remains visible.

- Every new stream lookup cancels the previous lookup and discards unselected results and their media caches. Current playback survives result refreshes and next-episode lookups until another stream is selected.
- A player's first master-playlist GET immediately marks **Playback detected**, before probing or alignment, and discards other candidates. HEAD checks are visible separately and do not consume the list.
- Playback links carry an encrypted, one-hour resume ticket. Stremio back-navigation and VLC handoff can recreate a discarded session without retaining its old media cache. The private `playback.key` beside the config also allows these links to survive a server restart. It is ignored by Git and saved with mode `0600`.
- Sessions expire after **one hour without media requests**, checked every 30 seconds. Cleanup cancels processing and removes associated audio/output caches and saved alignment data. Dashboard polling does not keep sessions alive. Small alignment records can be reused between lookups until they expire; media is never cached on disk.
- Unmodified upstream streams make no request to DubLift when selected, so their direct playback cannot be detected. Fully direct media also stops reporting playback position once its playlists are loaded.

## Supported media and bounded reads

| Input | Handling |
| --- | --- |
| Finite HTTP(S) HLS | Preserve source video playlists/segments. Rewrite or proxy only as needed. MPEG-TS, fMP4, AES-128, maps and byte ranges are supported. |
| Indexed Matroska | Read SeekHead, track metadata and video cues with finite HTTP ranges. Generate requested fMP4 video segments using stream copy. |
| Indexed MP4 | Read `moov`, timing and sync-sample tables; skip `mdat` while indexing. Generate requested fMP4 video segments using stream copy. |
| Vixsrc audio | Direct when eligible; otherwise short-window extraction and AAC MPEG-TS alternate renditions on the source clock. Italian is `DEFAULT=YES`. |
| Subtitles | Preserve original external/HLS subtitles where practical. Align Vixsrc WebVTT cues using the same offset timeline. |

File origins must return a valid `206 Partial Content` and `Content-Range` for `bytes=0-0`. Every later range is validated too. An origin that stops honoring ranges is rejected; there is no whole-file fallback.

FFmpeg sees only a finite audio window playlist or a capability-scoped ranged file endpoint. Container metadata reads are capped at 40 MiB, a media extraction has a 256 MiB origin-read budget, and individual output/media segments are capped at 96 MiB. Requested file ranges share the bounded cache, and playback reads take priority over background file analysis. The rolling in-memory cache defaults to 256 MiB; at most three FFmpeg/ffprobe jobs run at once, with background analysis limited to one slot so playback has capacity available. These limits are separate from temporary decoder/muxer memory. There is no background whole-title download or video cache on disk.

File video segments start at indexed keyframes, with a shorter first segment when the index permits. Remuxing excludes chapter text tracks, preserves HEVC sync flags, and uses compressed-packet timestamps to discard seek preroll without shifting the requested video's content. Video remains stream-copied. A provider transferring below the video's bitrate can still stall playback.

Matroska without usable video cues, MP4 without sample indexes or with complex edit lists, keyframe gaps above 40 seconds, live HLS, DRM/SAMPLE-AES and encrypted WebVTT are outside this MVP. Embedded file subtitles are not extracted. Original file audio is exposed as AAC renditions when practical. A player's video codec/HDR support still determines whether stream-copied video can be decoded.

## Android / Termux

Use current Termux and its platform FFmpeg package. Keep the executable under Termux's home directory, not shared Android storage (which may disallow execution).

```sh
pkg update
pkg install golang clang ffmpeg make
cd ~/DubLift
go build -trimpath -o bin/dublift ./cmd/dublift
./bin/dublift
```

Alternatively, `make release` on Linux builds `bin/dublift-android-arm64` alongside Linux binaries. Copy that binary and run it in Termux with FFmpeg installed. The pure-Go build uses Termux's `$PREFIX/etc/resolv.conf` when available. Native Termux builds can use Android's libc resolver. An explicit DNS resolver can be selected with `DUBLIFT_DNS=IP_ADDRESS` or `IP_ADDRESS:PORT` if the platform has no usable resolver configuration.

Optional `termux-wake-lock` keeps a proxied/remuxing session alive while the phone's screen is off. The core has no Android UI dependencies; an APK wrapper can supply executable paths, lifecycle handling and a private config directory.

## Tests and development checks

```sh
go test -race ./...
go vet ./...
go mod verify
make release
# Optional: installed VLC must decode video + Italian audio before/after seeking.
DUBLIFT_VLC_TESTS=1 go test ./internal/dublift -run TestVirtualHLSEndToEnd -v
```

The tests create synthetic media with FFmpeg and use local mock servers. They cover the scraper, episode IDs, range rejection, byte-range HLS, encryption keys, fingerprint verification, seeked MP4/MKV timestamps, generated-audio content, proxy preferences, manual controls, realignment persistence, cache limits, direct tolerance, both startup modes, full fallback-object preservation and ordering, playback detection, automatic session/cache expiry, canceled-job cleanup, and a seek after all DubLift listeners have stopped. FFmpeg integration tests skip explicitly if FFmpeg/ffprobe are unavailable. Stremio ExoPlayer, Nuvio and Android device testing remains a separate release check. See [validation results](docs/validation.md) for live provider and VLC checks.

FFmpeg 6.1's own HLS player has a [known fMP4 input-seek bug](https://ffmpeg.org/pipermail/ffmpeg-devel/2024-November/335634.html), which can report invalid NAL units or write empty output with `-ss ... -i ...m3u8`. DubLift's engine avoids that path by supplying short HLS windows. Tests verify late fMP4 segment selection, decoded frames and audio packets directly; MPEG-TS also exercises FFmpeg's input-seek path. VLC is checked separately.

To explicitly check the supplied development titles against configured providers:

```sh
go run ./cmd/dublift-check
```

This fetches stream lists and Vixsrc manifests and writes a private `.local/live-report.json`. Series use season 1, episode 1. Availability is reported rather than asserted. Normal tests never depend on third-party titles.

Protocol/library references: [Stremio HTTP addon protocol](https://stremio.github.io/stremio-addon-sdk/protocol.html), [gohlslib/v2](https://github.com/bluenviron/gohlslib), [gochromaprint](https://github.com/alexgorbatchev/gochromaprint), [FFmpeg formats](https://ffmpeg.org/ffmpeg-formats.html). `gochromaprint` is pinned to a checksum-verified commit because its `v1.6.0` tag failed the Go checksum verification during setup; checksum verification remains enabled.
