"use strict";
const $ = (id) => document.getElementById(id);
let settings,
  toastTimer,
  latestStatus,
  setupStep = -1,
  manifestBaseline = null;
const cards = new Map();
const addonDetailsCache = new Map();
const streamFilters = { italian: false, hls: false, mkv: false, mp4: false };
const streamFilterKey = "dublift.streamFilters.v1";
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
function lookupAddonDetails(url) {
  if (!addonDetailsCache.has(url)) {
    const request = api("/api/addon-name", { manifestURL: url });
    addonDetailsCache.set(url, request);
    const clear = () => {
      if (addonDetailsCache.get(url) === request) addonDetailsCache.delete(url);
    };
    request.then(clear, clear);
  }
  return addonDetailsCache.get(url);
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
const setText = (el, value) => {
  const text = String(value ?? "");
  if (el.textContent !== text) el.textContent = text;
};
const clock = (n) => {
  n = Math.max(0, Math.floor(n || 0));
  return [Math.floor(n / 3600), Math.floor(n / 60) % 60, n % 60]
    .map((v) => String(v).padStart(2, "0"))
    .join(":");
};
function completeAddonURL(input) {
  if (!input.value.trim() || !input.checkValidity()) return false;
  try {
    const url = new URL(input.value.trim());
    return ["http:", "https:"].includes(url.protocol) && url.pathname.endsWith("/manifest.json");
  } catch { return false; }
}
function syncAddonControls(container) {
  const rows = [...container.children];
  const button = container === $("addons") ? $("add-addon") : $("wizard-add-addon");
  button.disabled = rows.length >= 20 || rows.some((row) => !completeAddonURL(row.querySelector("input")));
  for (const row of rows) row.querySelector(".remove").hidden = rows.length <= 1;
}
function addonRow(addon = { name: "", manifestURL: "" }, container = $("addons")) {
  const row = document.createElement("div");
  row.className = "addon";
  const details = document.createElement("div");
  details.className = "addon-details";
  details.hidden = true;
  const icon = document.createElement("img");
  icon.className = "addon-icon";
  icon.alt = "";
  icon.hidden = true;
  const fallback = document.createElement("span");
  fallback.className = "addon-icon addon-icon-fallback";
  const name = document.createElement("strong");
  name.className = "addon-name";
  details.append(icon, fallback, name);
  row.append(details);
  const label = document.createElement("label");
  label.textContent = "Manifest URL";
  const input = document.createElement("input");
  input.type = "url";
  input.autocomplete = "url";
  input.required = true;
  input.value = addon.manifestURL;
  const status = document.createElement("p");
  status.className = "addon-status help";
  status.setAttribute("role", "status");
  status.hidden = true;
  let debounce;
  function showDetails(result) {
    row.dataset.name = result.name;
    name.textContent = result.name;
    fallback.textContent = result.name.charAt(0).toUpperCase();
    fallback.hidden = !!result.icon;
    icon.hidden = !result.icon;
    if (result.icon) icon.src = result.icon;
    details.hidden = false;
    status.hidden = true;
  }
  icon.onerror = () => { icon.hidden = true; fallback.hidden = false; };
  async function fetchDetails() {
    const url = input.value.trim();
    if (!url || !input.checkValidity()) return;
    status.textContent = "Checking addon…";
    status.hidden = false;
    try {
      const result = await lookupAddonDetails(url);
      if (input.value.trim() === url) showDetails(result);
    } catch (err) {
      if (input.value.trim() === url) {
        details.hidden = true;
        status.textContent = err.message;
        status.hidden = false;
      }
    }
  }
  input.addEventListener("input", () => {
    clearTimeout(debounce);
    details.hidden = true;
    status.hidden = true;
    row.dataset.name = "";
    if (input.checkValidity()) debounce = setTimeout(fetchDetails, 450);
    syncAddonControls(container);
  });
  input.addEventListener("change", () => {
    clearTimeout(debounce);
    fetchDetails();
    syncAddonControls(container);
  });
  label.append(input);
  row.append(label, status);
  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "text-button remove";
  remove.textContent = "Remove addon";
  remove.onclick = () => {
    if (container.children.length <= 1) return;
    row.remove();
    syncAddonControls(container);
  };
  row.append(remove);
  container.append(row);
  syncAddonControls(container);
  if (addon.name && addon.manifestURL) showDetails({ name: addon.name, icon: "" });
  if (addon.manifestURL) fetchDetails();
}
const readAddons = (container) => [...container.children].map((row) => ({
  name: row.dataset.name || "",
  manifestURL: row.querySelector("input").value.trim(),
}));
const inPlaybackSection = (session) => session.playing || session.preparationStarted;
function applyStreamFilters(sessions) {
  let shown = 0;
  let resultCount = 0;
  for (const session of sessions) {
    const card = cards.get(session.id);
    if (inPlaybackSection(session)) {
      card.hidden = false;
      continue;
    }
    resultCount++;
    const italian = session.checked !== false && !session.passthrough;
    const format = !streamFilters.hls && !streamFilters.mkv && !streamFilters.mp4 || !!streamFilters[session.sourceFormat];
    const matches = (!streamFilters.italian || italian) && format;
    card.hidden = !matches;
    if (matches) shown++;
  }
  $("empty").hidden = !!sessions.length;
  $("no-results").hidden = !sessions.length || resultCount > 0;
  $("filter-empty").hidden = !resultCount || shown > 0;
  setText($("filter-count"), resultCount
    ? `Showing ${shown} of ${resultCount}`
    : "");
}
function setStreamFilters(next) {
  for (const key of Object.keys(streamFilters)) {
    streamFilters[key] = next[key] === true;
    $("filter-" + key).setAttribute("aria-pressed", String(streamFilters[key]));
  }
  try { localStorage.setItem(streamFilterKey, JSON.stringify(streamFilters)); }
  catch { /* The filters still work for this page session. */ }
  if (latestStatus) applyStreamFilters(latestStatus.sessions);
}
try {
  const stored = JSON.parse(localStorage.getItem(streamFilterKey) || "{}");
  setStreamFilters(stored && typeof stored === "object" ? stored : {});
} catch { setStreamFilters({}); }
for (const key of Object.keys(streamFilters)) {
  $("filter-" + key).onclick = () =>
    setStreamFilters({ ...streamFilters, [key]: !streamFilters[key] });
}
$("clear-filters").onclick = () => setStreamFilters({});
const fields = {
  publicURL: "public-url",
  listen: "listen",
  searchRadius: "search-radius",
  alignmentSamples: "alignment-samples",
  maxItalianResults: "max-italian-results",
  maxItalianResultsPerStreamType: "max-italian-results-per-stream-type",
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
  settings.addons.forEach((addon) => addonRow(addon));
  if (!settings.addons.length) addonRow(undefined, $("addons"));
  $("wizard-addons").replaceChildren();
  settings.addons.forEach((addon) => addonRow(addon, $("wizard-addons")));
  if (!settings.addons.length) addonRow(undefined, $("wizard-addons"));
  $("wizard-max-results").value = settings.maxItalianResults;
  $("wizard-max-per-type").value = settings.maxItalianResultsPerStreamType;
  $("wizard-parallelism").value = settings.sourceCheckParallelism;
  $("wizard-timeout").value = settings.sourceCheckTimeoutSeconds;
  $("wizard-samples").value = settings.alignmentSamples;
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
$("wizard-add-addon").onclick = () => addonRow(undefined, $("wizard-addons"));
async function saveConfig(cfg) {
  settings = await api("/api/settings", cfg);
  await loadSettings();
}
$("settings-form").onsubmit = async (e) => {
  e.preventDefault();
  const cfg = { ...settings };
  for (const [key, id] of Object.entries(fields))
    cfg[key] = ["searchRadius", "alignmentSamples", "maxItalianResults", "maxItalianResultsPerStreamType", "sourceCheckTimeoutSeconds", "sourceCheckParallelism", "cacheMB"].includes(key)
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
  cfg.addons = readAddons($("addons"));
  try {
    await saveConfig(cfg);
    $("save-state").textContent = "Saved on this server.";
    toast("Settings saved");
  } catch (err) {
    toast(err.message);
  }
};
$("copy-manifest").onclick = () => copy($("manifest").value);
function showView(view) {
  $("streams-view").hidden = view !== "streams";
  $("settings-view").hidden = view !== "settings";
  for (const tab of document.querySelectorAll(".tab")) {
    const active = tab.dataset.view === view;
    tab.classList.toggle("active", active);
    if (active) tab.setAttribute("aria-current", "page");
    else tab.removeAttribute("aria-current");
  }
}
for (const tab of document.querySelectorAll(".tab")) {
  tab.onclick = () => showView(tab.dataset.view);
}
function renderWizard() {
  $("wizard").hidden = setupStep < 0;
  document.body.classList.toggle("setting-up", setupStep >= 0);
  if (setupStep < 0) return;
  $("wizard-progress").textContent = `Step ${setupStep + 1} of 3`;
  for (const section of document.querySelectorAll(".wizard-step"))
    section.hidden = Number(section.dataset.step) !== setupStep;
  for (const [index, label] of [...document.querySelectorAll(".wizard-steps span")].entries())
    label.classList.toggle("active", index === setupStep);
  $("wizard-back").hidden = setupStep === 0;
  $("wizard-next").textContent = setupStep === 2 ? "Finish setup" : "Continue";
  $("wizard-error").textContent = "";
}
function startWizard() {
  setupStep = 0;
  manifestBaseline = latestStatus?.manifestRequests ?? null;
  renderWizard();
  window.scrollTo({ top: 0, behavior: "smooth" });
}
$("run-setup").onclick = startWizard;
$("wizard-back").onclick = () => { setupStep--; renderWizard(); };
async function finishWizard() {
  try {
    await api("/api/setup-complete", {});
    settings.setupCompleted = true;
    setupStep = -1;
    renderWizard();
    showView("streams");
    toast("Setup complete. You can run it again from Settings.");
  } catch (err) { $("wizard-error").textContent = err.message; }
}
$("wizard-next").onclick = async () => {
  const button = $("wizard-next");
  button.disabled = true;
  try {
    if (setupStep === 0) {
      setupStep = 1;
      renderWizard();
    } else if (setupStep === 1) {
      const inputs = [...$("wizard-addons").querySelectorAll("input")];
      if (!inputs.length || inputs.some((input) => !completeAddonURL(input))) {
        $("wizard-error").textContent = "Enter an upstream HTTP(S) manifest URL ending in /manifest.json to continue.";
        inputs.find((input) => !completeAddonURL(input))?.focus();
        return;
      }
      await saveConfig({ ...settings, addons: readAddons($("wizard-addons")) });
      setupStep = 2;
      renderWizard();
    } else if (setupStep === 2) {
      const inputs = [...document.querySelectorAll(".wizard-fields input")];
      if (!inputs.every((input) => input.reportValidity())) return;
      await saveConfig({
        ...settings,
        maxItalianResults: Number($("wizard-max-results").value),
        maxItalianResultsPerStreamType: Number($("wizard-max-per-type").value),
        sourceCheckParallelism: Number($("wizard-parallelism").value),
        sourceCheckTimeoutSeconds: Number($("wizard-timeout").value),
        alignmentSamples: Number($("wizard-samples").value),
      });
      await finishWizard();
    }
  } catch (err) {
    $("wizard-error").textContent = err.message;
  } finally { button.disabled = false; }
};
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
    } catch (err) {
      toast(err.message);
    }
  };
  $(inPlaybackSection(session) ? "playing-sessions" : "sessions").append(card);
  cards.set(id, card);
  return card;
}
function playbackBadge(v) {
  if (v.passthrough) return "Original";
  if (v.playing) return "▶ Playback detected";
  if (v.preparationStarted) {
    if (!v.preparationDone) return "Preparing playback";
    return v.ready ? "Ready for playback" : "Preparation failed";
  }
  if (v.requestMethod === "HEAD") return "Player checked stream";
  return v.sourceCheckDeferred ? "Source unchecked" : "Available";
}
function updateCard(v) {
  const card = cards.get(v.id) || createCard(v);
  card.dataset.url = v.url;
  const set = (sel, value) => setText(card.querySelector(sel), value);
  set(".content", v.content);
  set(".content-name", v.contentName || v.title || v.content);
  set(".stream-title", v.title || "");
  set(".description", v.description || "");
  set(".file-details", v.filename || "");
  set(".playback-badge", playbackBadge(v));
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
  card.querySelector(".prepare").hidden = !!v.preparationStarted || !!v.ready || !!v.passthrough;
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
  const errorKey = JSON.stringify(v.errors || []);
  if (errors.dataset.value !== errorKey) {
    errors.dataset.value = errorKey;
    errors.replaceChildren();
    for (const text of v.errors || []) {
      const li = document.createElement("li");
      li.textContent = text;
      errors.append(li);
    }
  }
}
let lastEvents = "";
function renderStatus(state) {
    latestStatus = state;
    setText($("connection"), "Connected to local server");
    $("connection-dot").classList.remove("offline");
    if ($("manifest").value !== state.manifestURL) $("manifest").value = state.manifestURL;
    $("install").href = state.manifestURL.replace(/^https?:\/\//, "stremio://");
    setText($("session-count"), `${state.sessions.filter(v => v.playing).length} / ${state.sessions.length}`);
    setText($("italian-count"), `${state.sessions.filter(v => v.checked !== false && !v.passthrough).length} / ${state.sessions.length}`);
    setText($("lookup-status"), state.lookupStatus || "Waiting for an addon request or a title lookup.");
    setText($("cache-size"), `${mb(state.cacheBytes)} / ${mb(state.cacheMaxBytes)}`);
    setText($("origin-bytes"), mb(state.originBytes));
    const live = new Set(state.sessions.map((v) => v.id));
    for (const [id, card] of cards) {
      if (!live.has(id)) {
        card.remove();
        cards.delete(id);
      }
    }
    for (const session of state.sessions) {
      updateCard(session);
      const target = $(inPlaybackSection(session) ? "playing-sessions" : "sessions");
      const card = cards.get(session.id);
      if (card.parentElement !== target) target.append(card);
    }
    $("playing-section").hidden = !state.sessions.some(inPlaybackSection);
    applyStreamFilters(state.sessions);
    const events = state.events.length ? state.events : ["No events yet."];
    const key = JSON.stringify(events);
    if (key !== lastEvents) {
      lastEvents = key;
      $("events").replaceChildren();
      for (const event of events) {
        const li = document.createElement("li");
        li.textContent = event;
        $("events").append(li);
      }
    }
    if (setupStep === 0) {
      if (manifestBaseline === null) manifestBaseline = state.manifestRequests;
      else if (state.manifestRequests > manifestBaseline) {
        $("manifest-wait").textContent = "Manifest request detected.";
        setupStep = 1;
        renderWizard();
      }
    }
}
loadSettings().then(() => {
  if (!settings.setupCompleted) startWizard();
}).catch((err) => toast(err.message));
const statusStream = new EventSource("/api/events");
statusStream.onmessage = (event) => {
  try { renderStatus(JSON.parse(event.data)); }
  catch (err) { toast(`Invalid status update: ${err.message}`); }
};
statusStream.onerror = () => {
    setText($("connection"), "Server unreachable");
    $("connection-dot").classList.add("offline");
};
