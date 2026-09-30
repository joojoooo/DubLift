# DubLift

A local Stremio/Nuvio HTTP addon that combines an upstream stream's video with Vixsrc Italian and English audio. Go serves the addon, dashboard, media proxy, alignment engine and media endpoints. FFmpeg handles bounded audio extraction, AAC encoding and video **stream copy**. The supplied Vixsrc scraper’s resolver is ported to Go; no JavaScript runtime is needed.

## Start on Linux

Install Go **1.26.2 or newer**, FFmpeg and ffprobe, then:

```sh
go build -trimpath -o bin/dublift ./cmd/dublift
./bin/dublift
```

Open **http://localhost:7000**. The guided setup helps install DubLift in Stremio and connect upstream addons; it reads addon names from their manifests. The Public base URL defaults to the machine's LAN IPv4 address when one is available, so the manifest and playback links work on other devices. If no LAN address is found, set the URL in Settings. The wizard can be rerun from Settings. VLC can open a session's HLS URL from the Streams view.

Streams and Settings have separate dashboard views. Live playback and cache diagnostics arrive through a server-sent event stream, so the page does not repeatedly reload status while you select or copy text. The Streams view shows the Italian result count and can filter results to Italian links, HLS, MKV, or MP4; filter choices are saved in that browser.

Settings default to `.local/config.json`, with mode `0600`. The local configuration directory, alignment database, generated binaries, development reports and credentials are ignored by Git. Fresh installations have no upstream URLs. This workspace's supplied upstream was placed only in its private local configuration.

```sh
./bin/dublift -config /path/to/private/config.json
./bin/dublift -listen 127.0.0.1:7000
```

The default bind is `0.0.0.0:7000`. The dashboard is intended for a trusted local network. Its settings API rejects cross-origin browser requests; addon and media endpoints provide normal Stremio CORS. There is no built-in remote-access authentication.

## Playback through DubLift

DubLift serves every transformed playback playlist and media request from its local URL. It proxies HLS video segments, encryption keys, initialization maps and original HLS renditions with the required origin headers and cookies. File video uses local stream-copy remuxing; Vixsrc audio uses local extraction and aligned AAC segments. Keep DubLift running while playing or seeking a transformed stream. Reopen playback after upgrading so the player loads the local playlists.

**Start immediately with offset 0** defaults on. Once essential source probing finishes, playback begins while alignment continues in the background. Each check matches a short upstream English clip (default 5 seconds; 5–40 seconds) inside a longer Vixsrc audio search (default radius 10 seconds). At the defaults, this compares about 5 seconds of the upstream audio against 25 seconds of Vixsrc audio. For MKV and MP4 files, automatic alignment follows the player's video position and waits for completed video segments covering the short upstream clip, normally through about 15 seconds from the selected playback position. Its source extraction first uses file bytes already downloaded for playback. If the video remux did not read an interleaved audio range, alignment fetches the missing bytes after foreground file reads finish; a seek before the clip is ready moves the check to the new position. Upcoming generated audio segments adopt the offset after analysis finishes. With this option off, the first source read starts at 00:00 and gates playback; later samples continue in the background. A failed first attempt releases playback with offset zero.

## Synchronization

- English-to-English matching uses `gochromaprint`, then band-energy correlation and PCM refinement. A short clip from the upstream source is matched within the configured Vixsrc search range.
- **Alignment samples** is configurable from 1 to 12 (default 1), spread from near the beginning through later playback. The upstream clip defaults to **5 seconds** and the Vixsrc search radius to **10 seconds**, with a 30-second analysis limit per check. Existing custom settings are preserved. Consistent anchors produce one offset. When editions differ, playback uses the reliable match nearest the beginning.
- Confidence below the configured threshold, or a missing English reference, produces offset-zero fallback. Streams without usable Italian audio are returned exactly as received from the upstream addon.
- Positive offsets delay Vixsrc relative to the source. Manual offset overrides are applied to upcoming generated audio/subtitle segments; seek or reopen to flush audio already buffered by the player.
- **Realign from current position** uses the most recently requested local media segment, including seeks.
- Accepted realignments become stored timeline boundaries. Sections remain at normal speed; video is never time-stretched.
- Alignment and boundaries persist next to the config in `alignment.json`, keyed by content and source identity. Exact signed URLs and container/playlist identity avoid accidentally reusing another edition's sync; changed signed URLs can require a fresh analysis.

## Stream results and automatic cleanup

Stream discovery always confirms that Vixsrc has Italian audio. By default it then checks source manifests/range indexes and small prefixes of the first HLS video segment and initialization map, without decoding source audio. It returns up to the configured maximum of Italian results (default 3). The per-stream-type limit (default 2) groups sources by whether DubLift remuxes the video: MKV and MP4 files share the remuxed type, while HLS playlists, including those with MP4 fragments, share the other type. When remuxed sources rank first, the defaults give two of them Italian audio and reserve one slot for HLS. If HLS cannot fill that slot, an earlier remuxed source may use it. Results stay in upstream order. Each source check has a configurable timeout (default 20 seconds), and up to the configured number of checks run in parallel (default 3). Later entries keep their original links. With **Bypass source checks when listing** enabled, eligible HTTP streams get Italian result links once Vixsrc Italian audio is confirmed, subject to the same limits; each source is checked when selected for playback or when **Check & prepare** is clicked on the dashboard. Parallel source checks apply only during normal listing. Eligible entries retain the upstream name, title, description and provider details, with only a 🇮🇹 prefix added to the name. Torrents, external players and streams without Vixsrc Italian audio retain their original objects and links in either mode. In normal listing mode, unavailable video resources and non-seekable files also retain their original links. In bypass mode, those source failures are discovered when the stream is prepared. The exact external URL `https://pengu.uk/donate` is omitted from both stream results and the dashboard. If video probing fails later during preparation, the selected DubLift URL reports the failure; it does not publish an audio-only replacement.

The dashboard preserves the exact response order: configured-addon order, then each addon's original stream order. It displays full stream text and resolves the movie/series name through addon metadata, Cinemeta for IMDb IDs, or an optional TMDB token. When metadata is unavailable, the upstream title/description remains visible.

The **Video download** stat shows the selected stream's upstream video receive rate in Mbps, updated about once a second during MKV/MP4 range reads and proxied HLS video reads. **0.00 Mbps** means a video request is active but no bytes arrived in the last second. **Idle** means no video request is active. The rate includes bounded preparation of upcoming segments and may pause while cached data is used. **Last requested area** follows video requests when available, so separately fetched audio/subtitles cannot move it backward. This is a request position, not the player's playback clock.

- Every new stream lookup cancels the previous lookup and discards unselected results and their media caches. Current playback survives result refreshes and next-episode lookups until another stream is selected.
- A player's first master-playlist GET immediately marks **Playback detected**, before probing or alignment, and discards other candidates. HEAD checks are visible separately and do not consume the list.
- Playback links carry an encrypted, one-hour resume ticket. Stremio back-navigation and VLC handoff can recreate a discarded session without retaining its old media cache. The private `playback.key` beside the config also allows these links to survive a server restart. It is ignored by Git and saved with mode `0600`.
- Sessions expire after **one hour without media requests**, checked every 30 seconds. Cleanup cancels processing and removes associated audio/output caches and saved alignment data. Dashboard polling does not keep sessions alive. Small alignment records can be reused between lookups until they expire; media is never cached on disk.
- Unmodified upstream streams make no request to DubLift when selected, so their playback cannot be detected.

## Supported media and bounded reads

| Input | Handling |
| --- | --- |
| Finite HTTP(S) HLS | Rewrite source playlists and proxy segments and resources. MPEG-TS, fMP4, AES-128, maps and byte ranges are supported. |
| Indexed Matroska | Read SeekHead, track metadata and video cues with finite HTTP ranges. Generate requested fMP4 video segments using stream copy. |
| Indexed MP4 | Read `moov`, timing and sync-sample tables; skip `mdat` while indexing. Generate requested fMP4 video segments using stream copy. |
| Vixsrc audio | Short-window extraction and AAC MPEG-TS alternate renditions on the source clock. Italian is `DEFAULT=YES`. |
| Subtitles | Preserve original external/HLS subtitles where practical. Align Vixsrc WebVTT cues using the same offset timeline. |

File origins must return a valid `206 Partial Content` and `Content-Range` for the bounded initial range (up to 64 KiB). That same response supplies the container prefix, so discovery does not make separate sniff and one-byte probe requests. Later ranges are validated too. File reads use the original link so redirectors can select a healthy CDN worker for each range; failed file chunks can reopen the entry URL on additional workers. A range attempt that has not returned headers within three seconds is retried once through the entry URL, retaining the normal timeout for the second attempt. A progressing file range can stay open beyond the general HTTP client's 45-second response limit; playback instead bounds each cache-chunk read and the overall remux job. The Matroska header and, when available, a bounded tail remain in memory after indexing. When the seek table fits within that tail, later remuxes do not need another CDN request for it. Playback reads resume the unread remainder of a stalled or broken cache chunk without exposing a truncated file to FFmpeg. An origin that still ignores ranges is rejected; there is no whole-file fallback.

FFmpeg sees only a finite audio window playlist or a capability-scoped ranged file endpoint. Container metadata reads are capped at 40 MiB, a media extraction has a 256 MiB origin-read budget, and individual output/media segments are capped at 96 MiB. Playback and alignment share overlapping file bytes through the bounded cache, even when FFmpeg requests different range boundaries; only missing bytes are fetched. Playback reads take priority over background file analysis. The rolling in-memory cache defaults to 256 MiB; at most three FFmpeg/ffprobe jobs run at once, with background analysis limited to one slot so playback has capacity available. These limits are separate from temporary decoder/muxer memory. There is no background whole-title download or video cache on disk.

File video segments start at indexed cue points, with a shorter first segment when the index permits. Remuxing excludes chapter text tracks, preserves HEVC sync flags, and uses compressed-packet timestamps to discard seek preroll without shifting the requested video's content. If a file has cues but FFmpeg finds no marked key packets, DubLift retries stream copy with the cued packets included and divides the output at frame boundaries. Video remains stream-copied. One initialization map serves the file video playlist and stays available after earlier segments leave the cache. File remuxes retain bounded stream analysis to determine HEVC frame reordering and copy the demuxer timebase so B-frame presentation timestamps survive remuxing. Each HLS segment contains one MP4 fragment with a stable, increasing sequence number across remux windows and seeks. Each sequential remux prepares up to 30 seconds (targeting at most 24 MiB of source data, or one larger segment) and releases complete segments as they arrive. Bounded origin ranges of up to 32 MiB feed FFmpeg and the shared cache in 1 MiB chunks for video and original file audio, avoiding repeated redirects for individual chunks and segments. A demuxer closing a read immediately cancels the corresponding origin request. Incomplete video segments are retried instead of being cached.

Rolling workers prepare up to 60 seconds of upcoming file video and selected generated audio. The file window shrinks with the configured cache size and estimated source bitrate to leave room for source bytes, output segments, and audio. Seeking cancels obsolete work, including when a player requests audio at the seek target before video; failures and evicted entries remain eligible for preparation. Downloading pauses when the window is full and resumes as requests advance. The buffer stays in memory within the existing cache limit. A provider transferring below the video's bitrate can still stall playback.

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

The tests create synthetic media with FFmpeg and use local mock servers. They cover the scraper, episode IDs, range rejection, byte-range HLS, encryption keys, fingerprint verification, seeked MP4/MKV timestamps, generated-audio content, proxied HLS media, manual controls, realignment persistence, cache limits, both startup modes, full fallback-object preservation and ordering, playback detection, automatic session/cache expiry, canceled-job cleanup, and playback through the local proxy. FFmpeg integration tests skip explicitly if FFmpeg/ffprobe are unavailable. Stremio ExoPlayer, Nuvio and Android device testing remains a separate release check. See [validation results](docs/validation.md) for live provider and VLC checks.

FFmpeg 6.1's own HLS player has a [known fMP4 input-seek bug](https://ffmpeg.org/pipermail/ffmpeg-devel/2024-November/335634.html), which can report invalid NAL units or write empty output with `-ss ... -i ...m3u8`. DubLift's engine avoids that path by supplying short HLS windows. Tests verify late fMP4 segment selection, decoded frames and audio packets directly; MPEG-TS also exercises FFmpeg's input-seek path. VLC is checked separately.

To explicitly check the supplied development titles against configured providers:

```sh
go run ./cmd/dublift-check
```

This fetches stream lists and Vixsrc manifests and writes a private `.local/live-report.json`. Series use season 1, episode 1. Availability is reported rather than asserted. Normal tests never depend on third-party titles.

Protocol/library references: [Stremio HTTP addon protocol](https://stremio.github.io/stremio-addon-sdk/protocol.html), [gohlslib/v2](https://github.com/bluenviron/gohlslib), [gochromaprint](https://github.com/alexgorbatchev/gochromaprint), [FFmpeg formats](https://ffmpeg.org/ffmpeg-formats.html). `gochromaprint` is pinned to a checksum-verified commit because its `v1.6.0` tag failed the Go checksum verification during setup; checksum verification remains enabled.
