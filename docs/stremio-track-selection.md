# Stremio Android track selection

Stremio Android 2.5.1 can select the wrong HLS audio or subtitle track after
adding external subtitles. This was reproduced on a physical Android phone
using the player's bundled Media3 1.11.1-stremio01 code and a local SRT fixture.

The HLS player exposes alternative renditions with null format IDs. Without
external subtitles, Stremio uses a group/track index as the selection ID.
Adding an external subtitle merges the media sources and gives each null-ID
HLS format the same `0:` ID. Stremio then selects the first audio or subtitle
with that ID. An Italian selection can therefore choose English, even though
the playlist URLs, names, and language tags are distinct and correct.

DubLift avoids triggering this merge with upstream sidecar attachments in
transformed stream responses. Vixsrc and original HLS subtitles remain in the
local master playlist. Upstream fallback responses and the original stream
stored in playback tickets remain unchanged. Other Stremio subtitle addons
can still trigger the merge independently of DubLift.

With this workaround, both audio-language and subtitle-language selection
were confirmed working in Stremio 2.5.1 on the connected Android phone using
a fresh Movy 1080p result from the PC-hosted DubLift server.

Removing HLS `CODECS` is not a fix: it gives muxed original audio a distinct
ID, but the added audio and HLS subtitle IDs still collide after merging.
DubLift retains codec metadata for player compatibility.

A complete player-side fix should identify tracks by their track-group ID
and track index, or otherwise disambiguate duplicate format IDs. Listing and
selection must use the same identity even after external subtitles are added.
Changing language preferences or renaming playlist renditions cannot repair
the duplicate IDs.

To verify a player fix, start a local HLS fixture with English and Italian
audio and subtitles, then add a local SRT subtitle. Switch between every
audio and subtitle rendition before and after the merge and after a seek.
Check the selected track itself, not just the displayed language label.
