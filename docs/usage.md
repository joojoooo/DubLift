# Usage guide

See the [README](../README.md) for installation and first setup.

## Configuration and network access

Settings are stored in `.local/config.json` by default. Fresh installs include
VixSrc and Movy and do not require an upstream addon. In **Settings → Source settings**,
edit the fixed cards’ **Vixsrc base URL** or **Movy base URL**, or add optional addon sources
using their HTTP(S) manifest URLs. Source changes apply to new lookups;
changing the listen address requires a restart.

Source changes save automatically. An addon is saved once its manifest loads;
source ordering, visibility, removal, and built-in base URL changes save as
you edit them. While one view saves sources, source editing in the other view
waits for synchronization. The dashboard shows an error if saving fails.
Loaded addon names and logos are reused until you reload the dashboard.
Other settings use **Save settings**.

Each source’s switch controls whether its videos appear in your
player. Switching off VixSrc videos still allows its Italian audio to be used for
other sources. Use the up/down arrows to set source order; results stay
grouped in that order and addon results retain their original ordering. The
X removes an addon source, even the last one. Built-in sources cannot be
removed. These controls are also available during setup.

In the dashboard's **Stream results**, **Sources** filters select VixSrc, Movy or
individual addons independently of **TYPE** filters. Selecting multiple
sources shows results from any of them; **All** removes only the source
filter. Both filter groups combine and are remembered in your browser.
**Clear filters** resets both groups. Playing and preparing streams stay visible
in **Playback** regardless of filters.

Dashboard results use one title across sources, whether requested with
**Resolve a title** or through Stremio. Presets supply their title directly;
custom IDs use addon title text until metadata is available. Repeated
title-only lines, including addon icons and release years, are hidden in
the stream details. Resolution, bitrate, codecs, and other details remain.
If no title is available, the heading shows the content type and ID.

VixSrc lists each available HLS quality with its resolution and bitrate
(preferably average bitrate), highest quality first. If its playlist has no
quality metadata, DubLift offers **Auto quality**. VixSrc playback preserves
its original audio and subtitles without audio alignment.

Movy supplies high-quality video, including 4K and 1080p, with Italian audio
taken from VixSrc and synchronized using the same controls as addon streams.
Known qualities below 1080p are hidden. Its advertised qualities appear highest
first; unlabelled HLS streams use **Auto quality** and remain available as
fallbacks alongside labelled results. The search checks four Movy servers at a
time and returns as soon as both 4K and 1080p are found, or all servers finish.
Once any supported result is found, the search stops after eight seconds of
server searching if a target quality is still missing. Without results, it
continues under the existing listing deadline. This timing excludes initial
metadata retrieval, and subtitle or other source lookups can add time before
the results appear. Source probing and audio synchronization wait for
preparation or playback.

The base URL controls Movy's title pages and player origin;
its source and subtitle services are handled by the built-in scraper.
Only supported file/HLS sources are listed; DASH and DRM sources are excluded.

The config stores `configVersion` (currently `1`) and an ordered `sources`
array. On startup, a missing or unsupported version replaces the entire
config with current defaults and starts the setup wizard again. No config
migrations are currently available.

```sh
./bin/dublift -config /path/to/private/config.json
./bin/dublift -listen 127.0.0.1:7000
```

**Public base URL** controls the advertised manifest and playback URLs. It
defaults to a LAN IPv4 address when one is available. Check it when using
another device, changing the port, or moving networks. Restricting the listen
address to loopback makes the server accessible only on its own machine.

Config, `alignment.json`, and `playback.key` contain private state and live
beside each other. The config and key use file mode `0600`; do not share them
or raw provider reports. Generated binaries and `.local/` are ignored by Git.

FFmpeg and ffprobe paths are configurable in Settings. DNS normally uses the
system resolver; `DUBLIFT_DNS` optionally selects a DNS server IP, with an
optional port (`IP:PORT`, or `[IPv6]:PORT`).

## Playback and synchronization

Movy and eligible addon HTTP(S) results receive a local DubLift URL when Vixsrc Italian
audio is available. Torrents, external-player links, and results without
Italian audio keep their original links. Source compatibility is checked
when you prepare or play a result, so a listed result can still fail playback.

If VLC's Snap build stops immediately with local HTTP connection errors,
enable **Connect using HTTP access instead of custom HTTP code** in VLC's
advanced preferences (search for `adaptive`), or launch VLC with
`--adaptive-use-access`.

Playback starts after source preparation while English-to-English audio
matching runs in the background. Reliable matches update upcoming audio.
Without an English reference or a sufficiently confident match, playback
uses zero offset and manual timing remains available.

- **Prepare playback** inspects the source and finishes alignment before you
  open it. A failed alignment leaves zero-offset playback available and
  retries during playback when an English reference exists.
- **Adjust audio timing** lets you set a manual delay or **Match audio at
  playback position**. The position follows media requests, including seeks;
  it is not the player's exact playback clock.
- Positive offsets delay Vixsrc audio. Seek or reopen playback after a change
  to flush audio already buffered by the player.
- **Remove manual delay** restores saved automatic timing. **Clear saved
  timing and recheck** removes manual timing and saved matches before analysis.
- In Settings, increase **Sample length** for more audio to compare or
  **Search radius** for a larger timing difference. **Alignment samples**
  checks additional positions; different edits may still need manual timing.

Vixsrc WebVTT subtitles follow the audio timing. Original external/HLS
subtitles are preserved where practical; embedded file subtitles are not
extracted.

Keep DubLift running while playing or seeking. Streams exposes the local HLS
URL for VLC. Selecting a different stream replaces active playback; refreshing
results preserves it. Sessions and caches expire after an hour without media
requests. Old playback links can expire: request fresh results if needed.

## Media requirements

| Source | Requirements |
| --- | --- |
| HLS | Finite HTTP(S) VOD; MPEG-TS or fMP4. AES-128, initialization maps, and byte ranges are supported. |
| MKV | Usable video cues and an origin that serves valid HTTP byte ranges. |
| MP4 | Usable video sample indexes and valid HTTP byte ranges; complex edit lists are unsupported. |
| Player | Support for the original video codec and HDR format. Video is not re-encoded. |

Live HLS, DRM/SAMPLE-AES, encrypted WebVTT, and file keyframe gaps above
40 seconds are unsupported. Sources that ignore byte ranges cannot use file
playback. Reads and the rolling media cache are bounded and kept in memory;
there is no whole-title download or disk media cache.

## Troubleshooting

- **Playback fails:** inspect the stream's dashboard error and **Technical
  details**, then try another source. Preparation failures can display an
  error clip; rendering it needs FFmpeg `lavfi`, `drawtext`, `libx264`, and a
  usable font on Linux.
- **Audio is out of sync:** match at the current position or set a manual
  delay, then seek/reopen. Different cuts may not match reliably.
- **Playback stalls:** check **Video download** and provider errors. Origin
  speed below the video's bitrate, quotas, or expired URLs can interrupt
  playback even if preparation succeeds. `Idle` means no video request is
  active; `0.00 Mbps` means a request is active without recent received bytes.
- **Another device cannot connect:** verify the LAN address, port, firewall,
  and Public base URL. The player must reach the server throughout playback.

For Android installation and app lifecycle help, use
[DubLiftApp](https://github.com/joojoooo/DubLiftApp). For contributor checks,
see [validation](validation.md).
