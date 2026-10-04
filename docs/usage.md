# Usage guide

See the [README](../README.md) for installation and first setup.

## Configuration and network access

Settings are stored in `.local/config.json` by default. Fresh installs have
no upstream addons: add their HTTP(S) manifest URLs through the guided setup
or **Settings**. Upstream changes apply to new lookups; changing the listen
address requires a restart.

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

Eligible HTTP(S) results receive a local DubLift URL when Vixsrc Italian
audio is available. Torrents, external-player links, and results without
Italian audio keep their original links. Source compatibility is checked
when you prepare or play a result, so a listed result can still fail playback.

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
