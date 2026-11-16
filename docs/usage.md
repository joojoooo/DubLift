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
Successful source results remain available when another source times out.

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

Results show their **Binge group** in **Technical details** when available.
VixSrc and Movy groups use the provider and quality, shared across servers.
These groups stay the same across episodes and restarts so players can match
the next episode. External addon groups returned by DubLift use
`dublift:<original-group>`, including original fallback links. For example,
`Addon:4K` becomes `dublift:Addon:4K`. This separates DubLift results from the
same addon installed directly in Stremio. The original group is kept exactly as
the suffix, including case and whitespace. Missing or empty groups stay missing
or empty.

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
metadata retrieval, and other source lookups can add time before the results
appear. Source probing and audio synchronization wait for
preparation or playback.

The base URL controls Movy's title pages and player origin;
its source service is handled by the built-in scraper. The scraper preserves
each Movy server's own subtitle links and does not search separate subtitle
catalogs. During transformed playback, these official subtitles appear as local
HLS tracks such as **English · Movy**, alongside original HLS and VixSrc
subtitles. Audio and subtitles can be selected independently in Stremio/Nuvio;
the transformed stream JSON no longer duplicates them as attachments. Original
fallback responses retain their subtitle attachments and provider metadata.
Only supported file/HLS sources are listed; DASH and DRM sources are excluded.

The config stores `configVersion` (currently `3`) and an ordered `sources`
array. On startup, older supported configs execute sequential migrations
(`v1 -> v2` preserves all user settings and resets `publicURL` to empty for
dynamic client resolution; `v2 -> v3` enables next-episode preparation with a
5-minute lead while preserving existing settings, sources, credentials and
setup). Missing or unsupported versions replace the entire config with current
defaults and start the setup wizard again.

```sh
./bin/dublift -config /path/to/private/config.json
./bin/dublift --app-listen 127.0.0.1:7000
```

`--app-listen` sets and locks the listen address for a host-managed launch. For
standalone use, change the listen address in Settings and restart DubLift.

**Public base URL** is an optional override in Advanced Settings (defaulting to
empty). When left blank, DubLift dynamically resolves base URLs from incoming
player request headers (`Host`, `X-Forwarded-*`), enabling simultaneous playback
across localhost, local network (LAN), VPNs (e.g. Tailscale), and reverse proxies
without reconfiguration when DHCP IPs change. Set an explicit base URL only when
behind a reverse proxy that does not forward standard host headers. Restricting
the listen address to loopback makes the server accessible only on its own machine.

Config, `alignment.json`, and `playback.key` contain private state and live
beside each other. The config and key use file mode `0600`; do not share them
or raw provider reports. Generated binaries and `.local/` are ignored by Git.

FFmpeg and ffprobe paths are configurable in Settings for standalone runs. A
host application can supply and lock those paths and the listen address;
DubLift then hides their inputs. A host-provided cache default applies only to
new or reset configs and can be changed in Settings. DNS normally uses the
system resolver;
`DUBLIFT_DNS` optionally selects a DNS server IP, with an optional port
(`IP:PORT`, or `[IPv6]:PORT`).

## Automatic next-episode preparation

**Settings → Next episode → Automatically prepare the next episode** is enabled
by default. **Prepare before the end (minutes)** defaults to **5**, with a range
of **1–30**. These controls use **Save settings**; source autosaves and setup
preserve their saved values. Disabling stops outstanding automatic work and
clears unused results without stopping playback.

A selected series session triggers one attempt when its **last requested area**
reaches the interval before the known episode end. This is a media-request
position, not an exact playback clock: buffering can trigger early, and seeks
can jump into the interval. Video requests are authoritative once available;
audio requests supply the position until then. The dashboard need not be open.
Subtitle downloads do not update this position or trigger preparation.
Polling, HEAD probes, listing, preparation and background reads do not trigger
it. Movies, unknown/invalid durations or positions, missing binge groups and
untracked sources are ineligible. A seek back outside the interval cancels an
unused attempt; it does not rearm it. Failed, expired or consumed attempts also
do not retry within that selected session.

DubLift resolves an explicit next episode from addon episode metadata,
Cinemeta, or TMDB with a configured token, including season transitions. For
numeric IDs without a token, validated Movy title metadata can supply the IMDb
identity for Cinemeta. Unknown next episodes, specials without another known
special, and explicitly unreleased episodes are skipped. Incomplete or stale
catalogs can prevent prefetch; DubLift does not assume episode + 1 exists.

Only the **actual source connection** supplying the playing stream is searched
for next-episode streams, whether an addon, Movy or VixSrc. Metadata requests
and VixSrc Italian-audio dependencies may use other services. Results keep the
source's order and complete fallback objects, with only the player-facing binge
group namespace applied to addon results. The first stream with the
**exact same original upstream bingeGroup**, including case and whitespace, is
prepared using the same media preparation and audio alignment as **Prepare
playback**. No different group is substituted; an incompatible first match or failed preparation leaves
ordinary discovery available. Native VixSrc playback keeps its original tracks
and needs no alignment. Saved/manual timing, confidence checks and zero-offset
fallback follow the normal preparation rules.

The dashboard's **Automatic preparation → Up next** panel is visible only
while a series is playing and automatic preparation is enabled. It shows the
next episode, a short status message, and four preparation steps: finding the
episode, finding a stream, preparing video, and checking audio. While scheduled,
it explains when preparation starts or why it cannot start. A ready result shows
its audio timing status and expiry; a notice highlights audio that could not be
matched. Expand **Stream details** to see the chosen stream and audio delay.
The latest handoff, failure, cancellation or expiry remains visible for up to
45 minutes. Once the handed-off episode is selected for playback with GET, the
panel advances to the following episode in **Scheduled** state, using metadata
already fetched. This also works when the player opens a next-episode URL from
an earlier lookup, without requesting the prepared stream list again. This
clears the previous stream's preparation details without starting another search
or preparation. HEAD probes do not advance the panel.
If no following episode is known, the panel says so instead of guessing an ID.
This summary is separate from normal stream results and playback cards; viewing
it does not trigger work, consume a result or extend a session.

Automatic results stay separate from manual dashboard results. The **first GET** to
`/stream/series/{exact-next-id}.json` after successful preparation atomically
receives the restricted source results and their warmed sessions. This removes
the one-use response entry. Further requests perform normal discovery across
all enabled sources, as do dashboard **Resolve** requests and HEAD probes.
Responses disable caching. A miss, unfinished attempt, failure, cancellation or
expiry falls back immediately to ordinary discovery without waiting for the
automatic attempt. Playback URLs use the consuming request's host/proxy headers
or the configured Public base URL.

If an earlier next-episode URL selects the exact same source and media as the
ready prepared stream, DubLift adopts the warmed session and serves its child
playlists through that session. Adopted master playlists for transformed playback
keep any configured Public base URL path prefix. Repeated requests to that
earlier master stay usable for the selected session's lifetime. If a different stream is selected
for that episode, it follows normal playback preparation and unused speculative
work is discarded; the panel still advances to the following episode. Selecting
a different episode or title cancels the obsolete automatic preparation.

The addon protocol provides no reliable distinction between a manual Stremio
lookup and its automatic next-episode lookup: either can consume that first
eligible GET. Player variants may choose among duplicate matching groups
differently; DubLift follows [Stremio core’s first-match selection](https://github.com/Stremio/stremio-core/blob/development/src/models/player.rs)
and exact group comparison. Previously fetched stream lists cannot be replaced
proactively; warmed media reuse requires matching source/media identity.
Passthrough/external playback cannot report media positions to DubLift, so it
cannot trigger this feature.

One speculative worker and one unused result are kept for the current playback
context, with a **45-minute expiry from triggering**. Work yields between bounded
reads and alignment stages while foreground media requests are outstanding;
one background subprocess leaves engine capacity for foreground playback.
Automatic preparation rejects source lists above 200 entries instead of
truncating them; ordinary discovery remains available.
An already running bounded stage can finish before yielding. Preparation,
alignment download and calculation retain separate deadlines; waiting for
foreground work before an alignment stage does not use its deadline. There are
no whole-file downloads or general next-episode media prebuffers.

A valid addon **GET** to `/stream/{type}/{id}.json` for anything other than the
expected next episode cancels unused automatic work and clears its speculative
response and media cache. The new request uses ordinary discovery, while current
playback continues until a different stream is selected. Continued requests from
the old episode do not recreate that canceled attempt. Dashboard **Resolve**,
HEAD probes and invalid requests do not apply this rule. Once results have been
handed off, their playback URLs retain normal ownership through later lookups.
If episode metadata is still resolving, one requested identity is checked as
soon as the expected episode is known; two distinct addon lookups cancel that
unresolved attempt immediately rather than guessing an episode number.

Switching playback, discarding/expiring its parent, changing the relevant source
connection or alignment settings, disabling the feature, or shutting down
cancels unused work and results. Preparation alone never marks the next episode
as Playing or starts another prefetch. Handoff preserves the current episode
until the player selects the next master with GET; HEAD never selects it.
Handed-off sessions retain their prepared state through result refreshes and
old-parent cleanup, then follow normal playback selection and idle expiry.
Saved audio timing remains protected from cleanup while an unused prepared
session is waiting for handoff.

## Playback and synchronization

Movy and eligible addon HTTP(S) results receive a local DubLift URL when Vixsrc Italian
audio is available. If no track is identified as Italian, DubLift assumes the
first unknown-language audio track may be Italian and selects it by default.
These results also receive the Italian flag, but the track stays labelled
**Unknown · Vixsrc**. All unknown audio tracks remain available in the player's
audio menu so you can try another one. This also covers audio embedded in
VixSrc video playlists without separate audio renditions.

Torrents, external-player links, and results with neither Italian nor unknown
VixSrc audio keep their original links, without added VixSrc tracks or an Italian
flag. Source compatibility is checked when you prepare or play a result, so a
listed result can still fail playback.

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

Vixsrc WebVTT subtitles follow the audio timing. Official Movy subtitles keep
the original video's timeline, including after seeks or audio timing changes.
Original HLS subtitles are preserved; embedded file subtitles are not extracted.
Each generated standalone subtitle segment preserves the full timing of every
cue overlapping its playback window, including cues that cross its boundaries.

Standalone subtitle files support WebVTT and SRT encoded as UTF-8, with an
optional UTF-8 BOM and LF or CRLF line endings. Formats are detected from the
contents, so extensionless URLs work. Multiline cues and basic `<b>`, `<i>`, and
`<u>` emphasis are preserved. UTF-16/legacy encodings, HTML responses, ASS/SSA,
WebVTT STYLE/REGION blocks, and unsupported markup are rejected for that track.

Listing and preparation register up to 200 attachment entries without fetching
their bodies. A requested subtitle segment triggers a bounded download and text
conversion, shared across concurrent requests and reused while resident in the
session's rolling cache. Limits are 4 MiB per input file, 8 MiB for normalized
data or a generated segment, 20,000 cues, 8 KiB per line, and 64 KiB or 1,024
lines per cue. Cue timestamps may span up to seven days. Subtitle failures leave
video, audio, and other tracks usable; failed conversions can be retried.

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

Generated audio uses AAC-LC in fragmented MP4 with a shared initialization
segment. Players must support fMP4 HLS audio; segment retries and parallel
downloads preserve the same media clock. Original HLS renditions retain their
source format. DubLift retries a failed generated audio window once within the
request deadline, unless the provider's rate-limit retry budget is exhausted.
Failed source reads are rejected even if the encoder pads the missing audio
with silence.

## Troubleshooting

- **Playback fails:** inspect the stream's dashboard error and **Technical
  details**, then try another source. Preparation failures can display an
  error clip; rendering it needs FFmpeg `lavfi`, `drawtext`, `libx264`, and a
  usable font on Linux.
- **Audio is out of sync:** match at the current position or set a manual
  delay, then seek/reopen. Different cuts may not match reliably.
- **Audio briefly disappears:** an `incomplete encoded audio` error means the
  encoder returned fewer AAC frames than the playback window requires. A
  temporary audio-source failure can cause this; seeking back may succeed
  after the source recovers. If the bounded retry fails, the dashboard reports
  the error and recovery depends on the player's retry behavior.
- **Playback stalls:** check **Video download** and provider errors. Origin
  speed below the video's bitrate, quotas, or expired URLs can interrupt
  playback even if preparation succeeds. `Idle` means no video request is
  active; `0.00 Mbps` means a request is active without recent received bytes.
- **Another device cannot connect:** verify the host's LAN address, port,
  firewall, and network reachability. If an optional Public base URL override is
  configured, ensure it is reachable by the player. The player must reach the server throughout playback.

For Android installation and app lifecycle help, use
[DubLiftApp](https://github.com/joojoooo/DubLiftApp). For contributor checks,
see [validation](validation.md).
