# Handoff prompt: retain Movy subtitles inside DubLift's HLS playlist

## Task for the implementing AI

Implement support for exposing Movy's separate subtitle files as subtitle
renditions in DubLift's local HLS master playlist. Keep these subtitles
available without reintroducing the Stremio Android audio/subtitle selection
bug described below.

This document is a plan, not an implemented feature. The user explicitly
requested only this Markdown handoff at this stage. Do not treat the existence
of this document as authorization to begin implementation; proceed when the
user asks you to implement it.

Read the repository's `AGENTS.md` and `docs/validation.md` before changing code.
Inspect the current working tree: it contains the already verified workaround
and documentation from the preceding investigation. Preserve those changes.

## User-visible outcome

- Movy's subtitle languages appear in the internal player's subtitle menu,
  alongside VixSrc subtitles, with distinguishable labels.
- Italian and English VixSrc audio remain independently selectable, as do
  original audio and each subtitle rendition.
- Preferred-language selection continues to work where the player honors it.
- Seeking, source timing, and manual VixSrc timing adjustments remain correct.
- A broken subtitle file cannot prevent video/audio playback or hide working
  subtitle renditions.

## Confirmed diagnosis and existing workaround

The affected device runs Stremio 2.5.1 on an Android phone. The bundled player
reports Media3 `1.11.1-stremio01`. The user reproduced the problem with a Movy
1080p Interstellar result served by DubLift on their PC. Standalone Android
VLC could select the tracks correctly.

The investigation reproduced the following behavior using Stremio's actual
bundled player code on the phone and a local SRT fixture:

1. HLS alternative audio and subtitle renditions have null internal format IDs.
2. Before separately attaching subtitles, Stremio can distinguish these tracks
   using group/track indexes.
3. When Stremio attaches a separate subtitle file, Media3 merges the sources.
   The unnamed HLS formats acquire the same internal ID, `0:`.
4. Stremio's selection logic finds the first track of the requested type with
   that ID. Clicking Italian can therefore select English. Language names and
   playlist URLs remain distinct, but the player uses the duplicate IDs.

The verified workaround is in `Stream.dubbed` in `internal/dublift/session.go`:
it removes the top-level JSON `subtitles` attachments from the transformed
stream response. VixSrc and original HLS subtitles remain in the master
playlist. Original fallback stream objects and the original stream retained in
playback tickets keep their complete upstream metadata.

The user explicitly confirmed that both subtitle and audio selection work
after this workaround, using a fresh stream result on the phone.

Do **not** restore the top-level subtitle attachments to transformed stream
responses. Instead, recover them from the original `v.stream.Subtitles` and
serve them through the HLS track system.

Removing HLS `CODECS` was tested and did not fix the collision. Normal codec
metadata was restored and is needed for compatibility with VLC. Do not repeat
that approach or assume changing rendition names or IDs in the manifest fixes
Media3's format-ID handling.

An earlier theory about filtered versus unfiltered track indexes was not the
reproduced cause of this case. The external-subtitle merge and duplicate `0:`
IDs are the decisive evidence.

## Terminology

“Separate attachment” means a subtitle supplied through a stream's top-level
JSON `subtitles` list or by a separate Stremio subtitle addon. Stremio attaches
these to its player after opening the media.

“HLS subtitle rendition” means a subtitle declared by `EXT-X-MEDIA` in the
playback master playlist. Its data is also downloaded separately, but it is
part of the same playback description. This path avoids the problematic
separate-attachment merge when no other addon independently triggers it.

File extension alone does not determine which path is used. Movy's subtitle
download URLs can be extensionless. VixSrc currently supplies segmented
WebVTT; Movy attachments may require conversion from SRT.

## Relevant code

- `internal/dublift/movy.go`: `appendMovySubtitles` builds upstream subtitle
  entries with an ID, URL, and language. It validates URLs, deduplicates by
  URL/language, and caps the list at 200.
- `internal/dublift/session.go`: `Stream` preserves complete upstream JSON;
  `Stream.dubbed` removes separate attachments only from transformed responses;
  `prepareMedia` builds playback assets and tracks; `loadSubtitle` currently
  expects a WebVTT HLS media playlist, not an SRT file.
- `internal/dublift/engine.go`: `Track`, `Asset`, bounded subprocess helpers,
  and the media engine. Avoid treating a standalone subtitle file as a remote
  video file requiring media indexing.
- `internal/dublift/playback.go`: `master`, `generatedPlaylist`, track routes,
  `sectionClock`, and subtitle segment responses. Track URLs must remain local
  to DubLift. Original HLS renditions may take a proxy-playlist path; standalone
  subtitle files need the generated/converted path instead.
- `internal/dublift/subtitles.go`: WebVTT parsing, cue clipping, timestamp maps,
  source-clock handling, and VixSrc subtitle timing offsets.
- `internal/dublift/cache.go`: bounded caching, concurrent request coalescing,
  cancellation, owner scoping, and cache invalidation behavior.
- `internal/dublift/ticket.go`: original stream metadata survives resume tickets.
- `internal/dublift/lifecycle_test.go`: regression for upstream preservation,
  ordering, and omission of transformed stream attachments.
- `internal/dublift/proxy_test.go` and media integration tests: subtitle clocks,
  local playlists, and synthetic playback fixtures.
- `docs/stremio-track-selection.md`: current diagnosis and compatibility limits.

## Implementation plan

1. **Represent standalone subtitle tracks explicitly.** During preparation,
   register valid entries from the original stream's subtitle list as local
   subtitle tracks. Use a stable, namespaced ID based on source URL/language;
   do not rely on an upstream ID being unique or safe as a path component.
   Normalize language tags consistently with existing code. Use labels such
   as `Italian · Movy` and distinguish multiple tracks in the same language.
   Retain other languages rather than limiting support to English/Italian.

2. **Keep listing and preparation inexpensive.** Do not fetch subtitle bodies,
   inspect source media, or start conversion during stream listing. Prefer
   fetching a standalone subtitle only when a player requests one of its
   subtitle segments. Registration during preparation should not eagerly
   download up to 200 files. Deduplicate identical URL/language entries while
   preserving distinct tracks and upstream ordering where practical.

3. **Load and normalize text within explicit limits.** Detect SRT/WebVTT from
   bounded content, not just a URL suffix or response content type. Support
   both formats, including common BOMs, CRLF/LF, multiline cues, and ordinary
   text emphasis. Define handling for encodings and unsupported formats;
   inspect actual bounded provider samples if needed, keeping them private.
   Do not interpret an HTML error page as an empty successful subtitle.
   Either reuse a suitable existing conversion facility or add a focused text
   converter. Do not add a new dependency or launch FFmpeg unnecessarily for
   straightforward SRT-to-WebVTT conversion. If a subprocess is used, bound
   its input/output/runtime and stop it on cancellation using existing Android
   compatible process handling.

4. **Cache normalized content, then segment it locally.** A bounded complete
   subtitle text file can be fetched once and normalized; this is not a
   fallback to downloading a complete video. Bound downloaded bytes,
   decompressed/converted bytes, and parsing allocations. Use session-scoped
   cache contexts and existing concurrent-request coalescing. Failed or
   canceled work must not poison the cache or repopulate discarded state.
   Clip cues to requested playback windows, including cues spanning segment
   boundaries. A valid file with no cues in the requested window should return
   a valid empty WebVTT segment.

5. **Use the existing HLS rendition and media routes.** Add the new subtitle
   tracks to the master and generate a seekable VOD subtitle playlist with
   local WebVTT segment URLs. Reuse video/session boundaries and discontinuity
   handling where appropriate. Do not expose provider URLs, cookies, or signed
   query strings in generated playlists. Ensure these tracks do not enter the
   original-HLS proxy path by mistake. Keep the transformed JSON `subtitles`
   field absent so Stremio does not also attach the same files independently.

6. **Preserve the right timeline.** Movy sidecars belong to the original
   source's VOD timeline. They must not inherit VixSrc's automatic/manual audio
   offset. Their WebVTT timestamp maps must match the current video section's
   media clock, including nonzero initial PTS and discontinuity resets.
   Reuse existing clock logic carefully: an original standalone VOD subtitle
   is not necessarily an original HLS subtitle with an upstream timestamp map.
   Preserve existing VixSrc subtitle offset behavior.

7. **Preserve request semantics and isolate failures.** Use DubLift's network
   helpers, cancellation, redirect handling, and bounded reads. Respect
   subtitle-specific headers/cookies when available; do not blindly send
   video-origin authorization or cookies to an unrelated subtitle host.
   Invalid URLs or unsupported/unavailable files should fail only the affected
   track, with useful sanitized diagnostics. Do not fail source preparation or
   audio alignment because a subtitle attachment is broken.

8. **Keep fallback and resume behavior intact.** The original stream object,
   unknown JSON fields, original attachments, and playback ticket contents
   must remain intact. Reconstruct the same subtitle tracks after a resume
   ticket restores a session. Do not broaden this work into unrelated native
   VixSrc playback or changes to Stremio itself.

## Required regression coverage

Use local mock servers and synthetic media, with no third-party availability
in the ordinary suite. Cover observable behavior rather than merely checking
new helper implementation details.

- Upstream attachments become distinct local HLS renditions, while transformed
  JSON still has no separate attachments. Original fallback JSON remains exact.
- Listing and preparation do not eagerly fetch all subtitle files. Selecting
  one subtitle fetches only what is needed; parallel segment requests coalesce
  the file download/conversion, and later segments reuse cached content.
- SRT and WebVTT at extensionless URLs; BOM/line-ending handling; multiline
  cues; multiple same-language tracks; duplicate/missing upstream IDs;
  distinct English/Italian URLs and safe local track IDs.
- Startup, forward/backward seeks, empty windows, cues crossing boundaries,
  and final segments. Decode representative generated subtitle output with
  FFmpeg/ffprobe where useful, not just string matching.
- Nonzero video media clocks and discontinuity resets. A nonzero VixSrc
  manual/automatic offset changes VixSrc subtitles but does not shift Movy
  sidecars away from the original source timeline.
- Unavailable, malformed, oversized, and unsupported subtitle responses leave
  working video/audio and other subtitles usable. Assert bounded reads and
  sanitized errors. Check cancellation and cache invalidation without races.
- Restoring a session from its playback ticket retains the original subtitle
  list and reconstructs the same local tracks.

## Validation and device workflow

Follow the current `docs/validation.md`; run relevant focused tests, the full
race-enabled Go suite, vet, module verification, formatting/diff checks, and
local/Linux release builds as appropriate. Report actual skips and failures.
The preceding work found two pre-existing gofmt findings (extra trailing blank
lines) in `killprocess_test.go` and `killprocess_linux_test.go`; check whether
they still exist rather than attributing them to this change.

For this user's live setup:

- Start the **PC** server using `scripts/run.sh`. The phone's installed DubLift
  addon points to this PC's LAN IP. Do **not** launch the older DubLift Android
  app installed on the phone.
- An ADB-connected Android phone was available during diagnosis; rediscover
  its connection rather than hardcoding its changing wireless ADB port.
- Preserve the current running server until a tested replacement is ready.
  A transient user service named `dublift-stremio-test.service` was used to
  keep `scripts/run.sh` running independently of tool sessions; inspect its
  current state before starting a competing listener.
- Fetch a **fresh stream list** after rebuilding/restarting. A cached Stremio
  stream object can still contain old subtitle attachments even if its media
  URL restores successfully.
- Test a Movy 1080p Interstellar result. Verify original audio, VixSrc English,
  VixSrc Italian, VixSrc subtitles, and retained Movy subtitles independently,
  at startup and after seeks. Check both displayed selection and actual output.
- Verify the actual active player backend. During diagnosis, MPV failed GPU
  initialization and fell back to ExoPlayer; a menu preference alone does not
  prove an independent MPV playback test. Exercise available internal backends
  and standalone VLC where practical, reporting coverage accurately.

Other Stremio subtitle addons can still independently inject separate files
and trigger the underlying player bug. Do not claim this DubLift change fixes
Stremio's general merge behavior or alter the user's other addons to hide it.

## Documentation and completion criteria

Update `docs/usage.md`, `docs/stremio-track-selection.md`, and affected validation
guidance to explain that Movy's supported standalone subtitle files are now
served inside the HLS playlist rather than omitted. State supported formats,
real limits, and the remaining independent-addon limitation accurately.

The implementation is complete when local regressions pass, original fallback
objects remain intact, Movy's supported subtitles are available through local
HLS renditions, and the phone test confirms that retaining them does not break
independent audio/subtitle selection. If device testing is unavailable, report
that explicitly and do not claim physical-player verification.
