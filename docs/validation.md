# Validation — through 29 September 2026

## Deterministic checks

- Go tests, including the race detector; `go vet`; Go module checksum verification.
- Synthetic indexed MP4 and Matroska, including chaptered MKV and HEVC with open GOPs: ranged indexing, late video remux, compressed video stream copy, fragment presentation clocks, preserved keyframe flags and decoded frames.
- Synthetic MPEG-TS and fMP4 HLS, nonzero media sequences, byte ranges, AES-128 keys, header forwarding, source failures and bounded caching.
- English fingerprint matching plus correlation; configurable sample positions; manual overrides; persisted realignment boundaries; offset-zero fallback; immediate startup and first-sample startup gates; applying completed alignment to upcoming segments.
- Complete upstream JSON preservation for missing Italian, torrents, external links and non-seekable sources; emoji-only name changes; configured-addon/response ordering; full dashboard text.
- Playback detection before preparation completes; harmless HEAD checks; replacement of unselected results while preserving active playback; cancellation and cache collection; one-hour expiry of memory and disk alignment data; canceled work cannot restore deleted entries. Encrypted links recreate discarded sessions, including after a server restart; expired or tampered tickets are rejected.
- Requested file ranges share the bounded cache. Foreground reads can complete while analysis is paused, including when both request the same range; canceled analysis does not poison subsequent playback reads.
- Default ±0.125-second direct policy, failed/incomplete samples, differing edits, native clocks, discontinuities, infinite tolerance, setting persistence and proxy preference priority.
- A cached master continued to play and seek to encrypted alternate audio after both DubLift HTTP listeners were stopped. The mock origins stayed available; no DubLift redirect or media URL was needed.
- Headless Chromium: desktop and 390-pixel mobile layouts, settings save/reload, custom/infinite tolerance, upstream editing, episode presets, active-session delivery guidance, manual offset/reset and an explicit-position realignment request. New sample-count and startup controls were saved and reloaded. Full multiline stream details, original result order, selected-session removal, sample progress and zero-to-calculated offset updates were checked. No JavaScript errors or horizontal overflow.
- Linux amd64, Linux arm64 and Android arm64 builds. Android binary execution has not been tested on a physical device.

## Live provider checks

These are development observations, not availability guarantees. The private upstream configuration and raw local reports remain in ignored `.local/` files. No credentials are included here.

On 26 September, the development checker resolved upstream HTTP streams for all ten supplied titles. It inspected Vixsrc manifests only; it did not fetch complete videos. The following manifest results were observed:

| Title / TMDB ID | Vixsrc manifest observation |
| --- | --- |
| Devs / 81349, S1E1 | Media playlist; no separate rendition languages in the master-only report |
| Monster: The Lizzie Borden Story / 299939, S1E1 | Italian and English |
| Lanterns / 95350, S1E1 | Italian and English |
| House of the Dragon / 94997, S1E1 | Italian and English |
| Arcane / 94605, S1E1 | Italian and English |
| Project Hail Mary / 687163 | Italian and English |
| Coyote vs. Acme / 1204680 | Italian and English |
| The Matrix / 603 | Italian and English |
| Inception / 27205 | Italian and English |
| Interstellar / 157336 | Italian and English |

On 27 September, a Matrix 4K HLS source produced three verified anchors: +1.110 s, +1.124 s and +1.118 s, with confidence approximately 92%, 94% and 78%. The selected global offset was +1.118 s. Late video and Italian-audio segments were fetched and decoded. Anonymous origin checks rejected direct access for this source, so playback correctly retained header proxying.

A separate 37.7 GB indexed Matrix Matroska source passed strict range probing and exposed its original audio tracks. Its origin transferred a 4 MiB range in about 5.2 seconds, below the source's average media bitrate. After the fixes below, VLC decoded 4K HEVC video and Italian audio, but could not sustain playback or complete the seek within the test deadline. Requested video remuxes exceeded their 60-second limit. This source remains unsuitable on that measured connection; successful playlist generation is not counted as a playback pass.

The installed FFmpeg 6.1 HLS client reproduced its upstream fMP4 seek defect. Saved late fMP4 fragments decoded without errors. This client limitation is distinct from the server's bounded window extraction; the tests do not treat an empty successful FFmpeg exit as successful playback.

VLC 3.0.24 was exercised with dummy audio/video outputs through the ordinary local master URL. Its status reported decoded 3840×2160 H.264 video and Italian AAC audio while playback advanced past ten minutes. A seek to twenty minutes then produced 297 new decoded video frames and increased the decoded-audio counter by 832, with playback advancing past 20:02. DubLift reported no session errors. The private `.local/vlc-*-status.json` files contain the detailed playback counters.

Stremio ExoPlayer and Nuvio device playback, and Android/Termux runtime behavior, still need hardware validation. Direct URLs remain subject to their providers' expiry and availability after DubLift stops.

## Startup changes — live recheck

With immediate startup enabled, three samples and the existing custom 10-second search radius, a Matrix 4K HLS source returned its local master in **1.428 seconds** while the first alignment sample was still running. The dashboard marked playback before preparation finished and retained only the selected result. All three samples completed **33.85 seconds** after selection: offsets **+1.098**, **+1.142** and **+1.111 seconds**, with confidence approximately **88%**, **89%** and **79%**. The resulting offset was +1.111 seconds. A requested Italian segment returned 197,024 bytes in approximately 0.39 seconds and ffprobe identified AAC audio. This check fetched bounded manifests/audio windows and a requested segment; it did not download the title.

The stream lookup returned 25 entries in 12.43 seconds: 13 eligible transformations and 12 original fallback entries. Header/index/time-limit failures remained visible as original streams. These timings depend on provider/network conditions and verify server startup and bounded audio delivery. An earlier source with failed video probes incorrectly returned an audio-only master; the playback investigation below found and fixed that bug. Failed English alignment still permits offset-zero playback, but failed video probing no longer produces a transformed master.

## Playback failure investigation and VLC recheck

The installed `vlc` command was used to decode both tracks and seek, with dummy outputs and its HTTP interface reporting frame counters. Fixes included:

- Rejecting failed initial HLS video/init resources before advertising a transformed entry. One live Cinejoy/Lisbon source returned HTTP 502 for both video and initialization resources, reproducing audio-only playback. It now remains an unmodified upstream entry. A child-playlist error shadowing bug was also corrected.
- Preserving active playback during stream-result refreshes and restoring discarded sessions from encrypted playback links. A saved live stream URL played and sought after restarting the server.
- Excluding synthesized chapter text tracks from remuxed MP4, preserving `trun.first_sample_flags` for HEVC keyframes, and retaining actual packet timestamps so seek preroll is discarded rather than shifted into the requested segment.
- Sharing requested ranged file reads, avoiding unnecessary full stream-info scans for indexed file metadata, shortening the first video segment when possible, and prioritizing playback over file alignment reads.

Live Matrix checks: the final fresh-list 4K VidFast/vRapid HLS run decoded video and Italian audio in 6.13 seconds and passed a seek to ten minutes, producing 320 new video frames and 898 audio blocks. The 1080p HEVC PixelDrain file decoded both tracks in about 15–22 seconds across repeated runs; the final run passed the same seek with 287 new video frames and 678 audio blocks. Its inspected seek fragment covered 598.388–607.063 seconds against a declared clock of 598.388–607.064, with the first keyframe flag preserved. Session errors were empty on these successful checks. The large 4K MKV limitation above remains.

`DUBLIFT_VLC_TESTS=1 go test ./internal/dublift -run TestVirtualHLSEndToEnd -v` passed MKV, MPEG-TS HLS and fMP4 HLS playback, each requiring new video frames and Italian audio after a seek to 54 seconds. The complete race suite, `go vet`, module verification, and Linux/Android cross-builds passed. These are VLC and server checks; physical Stremio/Nuvio/Android player validation remains outstanding.

## 29 September — Lizzie Borden R2 playback

The 2.91 GB PenguPlay 1080p Matroska source for *Monster: The Lizzie Borden Story* S1E1 exposed an intermittent incomplete remux: one segment advertised 6.006 seconds but contained only about two seconds of video packets. A fresh remux of the same source produced all 144 expected frames. Italian audio timestamps were continuous. VLC on the live DubLift URL stalled and sometimes showed its time moving backward; the same generated video and Italian audio segments, served locally, advanced smoothly through 66 seconds.

File video range reads now keep a finite origin response open for up to 8 MiB while delivering and caching each 1 MiB chunk immediately. DubLift checks packet coverage, retries incomplete video output, publishes one stable fMP4 initialization map, and prepares at most three upcoming video segments with one bounded worker. The final live VLC run advanced from startup through 60 seconds in 63 seconds of wall time, with 2,920 decoded video frames, 5,784 decoded audio blocks, and no backward time jump. This checks the first minute, not the full episode. A synthetic truncated-fragment test, streaming-range test, and VLC playback/seek tests for MKV, MPEG-TS HLS and fMP4 HLS passed.

## 29 September — sustained file playback follow-up

The one-minute check above missed recurring stalls. An extended R2 run reproduced video requests taking 13–22 seconds: repeated six-second remuxes and origin reconnects eventually exhausted the player's buffer. Increasing lookahead alone did not resolve this. Audio and video requests also overwrote the same dashboard position, causing misleading backward movements.

File video now uses sequential remux windows of up to 30 seconds, sized toward 24 MiB of source data, publishing each complete segment while FFmpeg is still working. Finite origin requests stream up to 32 MiB in 1 MiB cache chunks. Indexed metadata avoids repeated stream-info scans. Rolling workers prepare video and the selected audio, retry failures, revisit evicted entries, and cancel obsolete reads/remuxes on seeks. The initialization map survives media-cache eviction. The dashboard follows video requests when visible and otherwise uses audio for direct-video playback.

An extended VLC run of the same 2.91 GB R2 source reached **479 seconds**. After playback began, its clock advanced **478 seconds in 478.21 seconds** of wall time, with **zero backward clock or requested-area jumps** and no observed rebuffering. The maximum unchanged-clock interval in one-second samples was 1.03 seconds. After the first 20 seconds, all 79 video responses completed within **5 ms** and all 80 selected Italian audio responses within **1 ms**. VLC reported 22,990 decoded video frames and 45,306 decoded audio blocks. Cache use stayed below its configured 512 MiB limit while old entries were evicted; the server process recorded zero disk writes. This is an eight-minute check, not a full-episode guarantee.

Regression checks cover progressive segment publication before the entire remux finishes, truncated output rejection, packet clocks and decoding for MP4/Matroska/chaptered Matroska/HEVC windows, bounded lookahead and retries, cancellation of abandoned origin reads, backward-seek state, and dashboard position arbitration. The full race suite, `go vet`, and VLC playback/seek tests for MKV, MPEG-TS HLS, and fMP4 HLS passed.
