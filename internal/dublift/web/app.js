"use strict";
const $ = (id) => document.getElementById(id);
let settings,
  toastTimer,
  polling = false;
const cards = new Map();
async function api(path, data) {
  const response = await fetch(
    path,
    data === undefined
      ? {}
      : {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(data),
        },
  );
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || `HTTP ${response.status}`);
  return result;
}
function toast(message) {
  $("toast").textContent = message;
  $("toast").style.display = "block";
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => ($("toast").style.display = "none"), 5000);
}
async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied to clipboard");
  } catch {
    const el = document.createElement("textarea");
    el.value = text;
    document.body.append(el);
    el.select();
    document.execCommand("copy");
    el.remove();
    toast("Copied to clipboard");
  }
}
const mb = (n) => (n / 1048576).toFixed(1) + " MiB";
const clock = (n) => {
  n = Math.max(0, Math.floor(n || 0));
  return [Math.floor(n / 3600), Math.floor(n / 60) % 60, n % 60]
    .map((v) => String(v).padStart(2, "0"))
    .join(":");
};
function addonRow(addon = { name: "", manifestURL: "" }) {
  const row = document.createElement("div");
  row.className = "addon";
  for (const [key, label, type] of [
    ["name", "Addon name", "text"],
    ["manifestURL", "Manifest URL · private", "password"],
  ]) {
    const l = document.createElement("label");
    l.textContent = label;
    const input = document.createElement("input");
    input.dataset.key = key;
    input.type = type;
    input.autocomplete = "off";
    input.required = true;
    input.value = addon[key];
    input.placeholder =
      key === "name" ? "My upstream addon" : "https://…/manifest.json";
    l.append(input);
    row.append(l);
  }
  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "text-button remove";
  remove.textContent = "Remove addon";
  remove.onclick = () => row.remove();
  row.append(remove);
  $("addons").append(row);
}
const fields = {
  publicURL: "public-url",
  listen: "listen",
  searchRadius: "search-radius",
  alignmentSamples: "alignment-samples",
  maxItalianResults: "max-italian-results",
  sourceCheckTimeoutSeconds: "source-check-timeout",
  sourceCheckParallelism: "source-check-parallelism",
  cacheMB: "cache-mb",
  ffmpeg: "ffmpeg",
  ffprobe: "ffprobe",
  vixBaseURL: "vix-base",
  tmdbToken: "tmdb-token",
};
async function loadSettings() {
  settings = await api("/api/settings");
  for (const [key, id] of Object.entries(fields))
    $(id).value = settings[key] ?? "";
  $("bypass-source-checks").checked = settings.bypassSourceChecks;
  $("prefer-proxy").checked = settings.preferProxy;
  $("start-immediately").checked = settings.startImmediately;
  $("direct-playback").checked = settings.directPlayback;
  $("direct-mode").value =
    settings.directTolerance === null ? "infinite" : "finite";
  $("direct-tolerance").value = settings.directTolerance ?? 0.125;
  directControls();
  $("confidence").value = settings.minConfidence;
  confidenceLabel();
  $("addons").replaceChildren();
  settings.addons.forEach(addonRow);
}
function confidenceLabel() {
  $("confidence-value").textContent =
    Math.round(Number($("confidence").value) * 100) + "%";
}
function directControls() {
  const infinite = $("direct-mode").value === "infinite";
  $("direct-tolerance-label").hidden = infinite;
  $("direct-tolerance").disabled = infinite || !$("direct-playback").checked;
  $("direct-mode").disabled = !$("direct-playback").checked;
}
$("direct-mode").onchange = directControls;
$("direct-playback").onchange = directControls;
$("confidence").oninput = confidenceLabel;
$("add-addon").onclick = () => addonRow();
$("settings-form").onsubmit = async (e) => {
  e.preventDefault();
  const cfg = { ...settings };
  for (const [key, id] of Object.entries(fields))
    cfg[key] = ["searchRadius", "alignmentSamples", "maxItalianResults", "sourceCheckTimeoutSeconds", "sourceCheckParallelism", "cacheMB"].includes(key)
      ? Number($(id).value)
      : $(id).value.trim();
  cfg.bypassSourceChecks = $("bypass-source-checks").checked;
  cfg.preferProxy = $("prefer-proxy").checked;
  cfg.startImmediately = $("start-immediately").checked;
  cfg.directPlayback = $("direct-playback").checked;
  cfg.directTolerance =
    $("direct-mode").value === "infinite"
      ? null
      : Number($("direct-tolerance").value);
  cfg.minConfidence = Number($("confidence").value);
  cfg.addons = [...$("addons").children].map((row) =>
    Object.fromEntries(
      [...row.querySelectorAll("input")].map((i) => [
        i.dataset.key,
        i.value.trim(),
      ]),
    ),
  );
  try {
    await api("/api/settings", cfg);
    settings = cfg;
    $("save-state").textContent = "Saved on this device.";
    toast("Settings saved");
  } catch (err) {
    toast(err.message);
  }
};
$("copy-manifest").onclick = () => copy($("manifest").value);
$("preset").onchange = () => {
  if (!$("preset").value) return;
  const [type, id] = $("preset").value.split(":");
  $("content-type").value = type;
  $("content-id").value = `tmdb:${id}${type === "series" ? ":1:1" : ""}`;
};
$("resolve-form").onsubmit = async (e) => {
  e.preventDefault();
  const button = e.target.querySelector("button");
  button.disabled = true;
  $("resolve-status").textContent = "Querying configured addons…";
  try {
    const r = await api("/api/resolve", {
      type: $("content-type").value,
      id: $("content-id").value.trim(),
    });
    const hint = !r.streams.length
      ? "Check the activity log for provider errors."
      : settings?.bypassSourceChecks
        ? "Prepare a session to check its source."
        : "Prepare a session to inspect audio.";
    $("resolve-status").textContent = `${r.streams.length} streams found. ${hint}`;
    await poll();
  } catch (err) {
    $("resolve-status").textContent = err.message;
  } finally {
    button.disabled = false;
  }
};
function createCard(session) {
  const card = $("session-template").content.firstElementChild.cloneNode(true);
  const id = session.id;
  card.dataset.id = id;
  card.querySelector(".copy").onclick = () => copy(card.dataset.url);
  const action = (selector, route, body = {}, message = "Request accepted") =>
    (card.querySelector(selector).onclick = async () => {
      const btn = card.querySelector(selector);
      btn.disabled = true;
      try {
        let payload = body;
        if (route === "realign") {
          const position = card.querySelector(".player-position").value;
          if (position !== "") payload = { position: Number(position) };
        }
        await api(`/api/sessions/${id}/${route}`, payload);
        toast(message);
        await poll();
      } catch (err) {
        toast(err.message);
      } finally {
        btn.disabled = false;
      }
    });
  action(".prepare", "prepare", {}, "Playback is ready");
  action(
    ".realign",
    "realign",
    {},
    "Matching English audio around the latest requested segment",
  );
  action(".analyze", "analyze", {}, "Alignment analysis started");
  action(
    ".automatic",
    "offset",
    { offset: null },
    "Automatic synchronization restored",
  );
  action(".reset", "reset", {}, "Saved offsets and boundaries reset");
  card.querySelector(".offset-form").onsubmit = async (e) => {
    e.preventDefault();
    const input = card.querySelector(".manual");
    if (input.value === "") return;
    try {
      await api(`/api/sessions/${id}/offset`, { offset: Number(input.value) });
      toast("Offset updated. Seek to refresh buffered audio.");
      await poll();
    } catch (err) {
      toast(err.message);
    }
  };
  $("sessions").append(card);
  cards.set(id, card);
  return card;
}
function updateCard(v) {
  const card = cards.get(v.id) || createCard(v);
  card.dataset.url = v.url;
  const set = (sel, text) => (card.querySelector(sel).textContent = text);
  set(".content", v.content);
  set(".content-name", v.contentName || v.title || v.content);
  set(".stream-title", v.title || "");
  set(".description", v.description || "");
  set(".file-details", v.filename || "");
  set(".playback-badge", v.passthrough ? "Original" : v.playing ? "▶ Playback detected" : v.requestMethod === "HEAD" ? "Player checked stream" : v.sourceCheckDeferred ? "Source unchecked" : "Available");
  card.classList.toggle("playing", !!v.playing);
  set(".sample-progress", v.aligning ? `Sample ${v.sampleIndex || 1} of ${v.sampleTotal || settings?.alignmentSamples || 3} · ${v.samplePhase || "Starting analysis"}` : v.samplePhase || "");
  set(".name", v.name || "Upstream stream");
  set(".status", v.status + (v.aligning ? " · analysis running" : ""));
  set(".position", clock(v.position) + " / " + clock(v.duration));
  const a = v.alignment || {};
  set(
    ".confidence",
    a.confidence ? Math.round(a.confidence * 100) + "%" : "Not matched",
  );
  const minimum = settings?.minConfidence ?? 0.68;
  let offset = a.manual ?? (v.startupZero && !a.automaticComplete ? 0 : a.confidence >= minimum ? a.offset : 0) ?? 0;
  if (a.manual == null) {
    for (const b of a.boundaries || []) {
      if (b.sourceTime <= v.position && b.confidence >= minimum) offset = b.offset;
    }
  }
  set(
    ".offset",
    `${offset >= 0 ? "+" : ""}${Number(offset).toFixed(3)} s${a.manual != null ? " · manual" : ""}`,
  );
  card.querySelector(".confidence-bar span").style.width =
    (a.confidence || 0) * 100 + "%";
  set(".tracks", (v.tracks || []).map((t) => t.name).join(" · "));
  set(".proxy", v.proxyReason || "");
  const direct = v.directPlayback || {};
  const loaded = v.loadedDelivery;
  const reload =
    loaded &&
    (loaded.videoDirect !== direct.videoDirect ||
      loaded.audioDirect !== direct.audioDirect ||
      loaded.canStopServer !== direct.canStopServer);
  set(
    ".delivery-mode",
    reload
      ? direct.canStopServer
        ? "Direct playback ready · reopen the HLS URL"
        : "Playback settings changed · reopen the HLS URL"
      : direct.canStopServer
        ? "Fully direct playback available"
        : direct.audioDirect
          ? "Direct audio · DubLift still needed"
          : direct.videoDirect
            ? "Direct video · generated audio"
            : "DubLift media processing",
  );
  set(
    ".delivery-reason",
    (direct.reasons || []).filter(Boolean).join(" ") +
      (direct.canStopServer
        ? " Once the player has loaded these playlists, playback and seeking use the origins. Signed URLs still have their normal expiry."
        : "") +
      (reload
        ? " Existing buffered playback keeps its previous URLs. Reopen to switch."
        : ""),
  );
  set(
    ".player-delay",
    direct.forced
      ? direct.playerOffset != null && a.confidence > 0
        ? `Suggested player audio delay: ${direct.playerOffset >= 0 ? "+" : ""}${direct.playerOffset.toFixed(3)} s. This includes the native media clock difference.`
        : "Infinite tolerance is active. No verified player delay is available yet."
      : "",
  );
  card.querySelector(".player-position-label").hidden = !(
    loaded?.canStopServer || direct.canStopServer
  );
  card.querySelector(".prepare").textContent = v.sourceCheckDeferred ? "Check & prepare" : "Prepare playback";
  card.querySelector(".prepare").hidden = !!v.ready || !!v.passthrough;
  card.querySelector(".prepare").disabled = v.checked === false;
  card.querySelector(".session-metrics").hidden = !v.ready;
  card.querySelector(".confidence-bar").hidden = !v.ready;
  card.querySelector(".delivery-info").hidden = !v.ready;
  card.querySelector("details").hidden = !v.ready;
  card.querySelector(".realign").hidden = !v.ready;
  card.querySelector(".copy").textContent = v.passthrough ? "Copy original URL" : "Copy HLS URL";
  if (v.passthrough) set(".sample-progress", `${v.fallbackReason || "DubLift unavailable"}. Sent unchanged to Stremio. Direct upstream playback does not report activity to DubLift.`);
  card.querySelector(".realign").disabled = v.aligning || !v.ready;
  card.querySelector(".analyze").disabled = v.aligning || !v.ready;
  const input = card.querySelector(".manual");
  if (document.activeElement !== input) input.value = a.manual ?? "";
  set(
    ".anchors",
    [
      a.differentEdit
        ? "Edit differences detected. Using the earliest reliable match."
        : "",
      ...(a.anchors || []).map(
        (x) =>
          `Sample ${clock(x.vixTime)} → ${clock(x.sourceTime)} · ${x.offset.toFixed(3)} s · ${Math.round(x.confidence * 100)}%`,
      ),
      ...(a.boundaries || []).map(
        (x) =>
          `Saved boundary from ${clock(x.sourceTime)} · ${x.offset.toFixed(3)} s`,
      ),
    ]
      .filter(Boolean)
      .join("\n"),
  );
  const errors = card.querySelector(".errors");
  errors.replaceChildren();
  for (const text of v.errors || []) {
    const li = document.createElement("li");
    li.textContent = text;
    errors.append(li);
  }
}
async function poll() {
  if (polling) return;
  polling = true;
  try {
    const state = await api("/api/status");
    $("connection").textContent = "Connected to local server";
    $("connection-dot").classList.remove("offline");
    $("manifest").value = state.manifestURL;
    $("install").href = state.manifestURL.replace(/^https?:\/\//, "stremio://");
    $("engine-state").textContent =
      state.ffmpeg && state.ffprobe ? "FFmpeg ready" : "Dependency missing";
    $("session-count").textContent = `${state.sessions.filter(v => v.playing).length} / ${state.sessions.length}`;
    $("lookup-status").textContent = state.lookupStatus || "Waiting for an addon request or a title lookup.";
    $("cache-size").textContent = mb(state.cacheBytes);
    $("origin-bytes").textContent = mb(state.originBytes);
    $("empty").hidden = !!state.sessions.length;
    const live = new Set(state.sessions.map((v) => v.id));
    for (const [id, card] of cards) {
      if (!live.has(id)) {
        card.remove();
        cards.delete(id);
      }
    }
    state.sessions.forEach(updateCard);
    $("events").replaceChildren();
    for (const event of state.events.length
      ? state.events
      : ["No events yet."]) {
      const li = document.createElement("li");
      li.textContent = event;
      $("events").append(li);
    }
  } catch (err) {
    $("connection").textContent = "Server unreachable";
    $("connection-dot").classList.add("offline");
  } finally {
    polling = false;
  }
}
loadSettings().catch((err) => toast(err.message));
poll();
setInterval(poll, 1000);
