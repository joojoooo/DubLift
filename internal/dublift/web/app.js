"use strict";
const $ = (id) => document.getElementById(id);
let sourceTypes = [];
let settingsWrite = Promise.resolve();
const sourceWrites = new Map();
let settings,
  toastTimer,
  latestStatus,
  setupStep = -1,
  manifestBaseline = null;
const cards = new Map();
const addonDetailsCache = new Map();
const addonDetailsRequests = new Map();
const streamFilters = { italian: false, hls: false, mkv: false, mp4: false };
const streamFilterKey = "dublift.streamFilters.v1";
const sourceFilters = new Set();
const sourceFilterKey = "dublift.sourceFilters.v1";
const sourceFilterButtons = new Map();
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
  if (addonDetailsCache.has(url)) return Promise.resolve(addonDetailsCache.get(url));
  if (!addonDetailsRequests.has(url)) {
    const request = api("/api/addon-name", { manifestURL: url }).then((result) => {
      addonDetailsCache.set(url, result);
      // Bound successful lookups, including URLs edited during this page visit.
      if (addonDetailsCache.size > 64) addonDetailsCache.delete(addonDetailsCache.keys().next().value);
      return result;
    });
    addonDetailsRequests.set(url, request);
    const clear = () => {
      if (addonDetailsRequests.get(url) === request) addonDetailsRequests.delete(url);
    };
    request.then(clear, clear);
  }
  return addonDetailsRequests.get(url);
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
function completeSourceURL(row) {
  const input = row.querySelector("[data-source-url]");
  if (row.dataset.type === "addon") return completeAddonURL(input);
  try {
    return input.checkValidity() && ["http:", "https:"].includes(new URL(input.value.trim()).protocol);
  } catch { return false; }
}
function syncSourceControls(container) {
  const rows = [...container.children];
  const addons = rows.filter((row) => row.dataset.type === "addon");
  const button = container === $("sources") ? $("add-source") : $("wizard-add-source");
  const waiting = !!sourceWrites.get(otherSourceContainer(container));
  button.disabled = waiting || addons.length >= 20 || addons.some((row) => !completeSourceURL(row));
  rows.forEach((row, index) => {
    row.querySelector("[data-source-url]").disabled = waiting;
    row.querySelector(".source-enabled").disabled = waiting;
    const remove = row.querySelector(".source-remove");
    if (remove) remove.disabled = waiting;
    row.querySelector(".source-up").disabled = waiting || index === 0;
    row.querySelector(".source-down").disabled = waiting || index === rows.length - 1;
  });
}
function otherSourceContainer(container) {
  return container === $("sources") ? $("wizard-sources") : $("sources");
}
function queueSettingsWrite(write) {
  const request = settingsWrite.then(write);
  settingsWrite = request.catch(() => {});
  return request;
}
async function waitSettingsWrites() {
  let pending;
  do {
    pending = settingsWrite;
    await pending;
  } while (pending !== settingsWrite);
}
function queueSettingsSave(buildConfig) {
  return queueSettingsWrite(async () => {
    settings = await api("/api/settings", buildConfig());
    return settings;
  });
}
function renderSources(container, sources) {
  container.replaceChildren();
  sources.forEach((source) => sourceRow(source, container));
}
function sourceKey(source) {
  return source && (source.type === "addon" ? source.manifestURL : source.type);
}
function syncSources(container, sources) {
  // Queued writes own their source order and toggles until they finish.
  if (sourceWrites.get(container)) return;
  const drafts = [...container.children].flatMap((row, index) => {
    const current = readSource(row), saved = row.savedSource;
    const urlKey = current.type === "addon" ? "manifestURL" : "baseURL";
    return !saved || current[urlKey] !== saved[urlKey] || current.disabled !== !!saved.disabled
      ? [{ row, index }] : [];
  });
  container.replaceChildren();
  const retained = new Set();
  for (const source of sources) {
    const draft = drafts.find(({ row }) => !retained.has(row) &&
      [row.savedSource, row.validatedSource].some((previous) => sourceKey(previous) === sourceKey(source)));
    if (draft) {
      const previous = draft.row.savedSource;
      if (previous && readSource(draft.row).disabled === !!previous.disabled) {
        draft.row.querySelector(".source-enabled").checked = !source.disabled;
        draft.row.classList.toggle("source-disabled", !!source.disabled);
      }
      if (previous && sourceKey(draft.row.validatedSource) === sourceKey(previous)) {
        draft.row.validatedSource = { ...source };
      }
      draft.row.savedSource = { ...source };
      container.append(draft.row);
      retained.add(draft.row);
    } else sourceRow(source, container);
  }
  for (const { row, index } of drafts) {
    if (retained.has(row)) continue;
    if (index < container.children.length) container.insertBefore(row, container.children[index]);
    else container.append(row);
  }
  syncSourceControls(container);
}
async function queueSourceSave(container, buildConfig, entries, syncCurrent = false) {
  sourceWrites.set(container, (sourceWrites.get(container) || 0) + 1);
  const other = otherSourceContainer(container);
  syncSourceControls(other);
  let saved;
  try {
    saved = await queueSettingsSave(buildConfig);
    entries.forEach(({ row }, index) => { row.savedSource = { ...saved.sources[index] }; });
  } finally {
    sourceWrites.set(container, sourceWrites.get(container) - 1);
    syncSourceControls(other);
  }
  syncSources(other, saved.sources);
  if (syncCurrent) syncSources(container, saved.sources);
  return saved;
}
async function saveSources(container) {
  // The other view may still be saving a source addition or removal. Its
  // controls are disabled, but an earlier manifest lookup can finish now.
  // Wait for synchronization before collecting this view's source list.
  if (sourceWrites.get(otherSourceContainer(container))) await waitSettingsWrites();
  // Keep incomplete URL edits as drafts. Reordering or toggling other cards
  // must still save, retaining each source's last validated URL, including
  // changes already queued for saving.
  const entries = [...container.children].flatMap((row) => {
    const current = readSource(row);
    const valid = completeSourceURL(row) && (current.type !== "addon" || row.dataset.validatedURL === current.manifestURL);
    if (valid) row.validatedSource = { ...current };
    const previous = row.validatedSource || row.savedSource;
    const source = valid ? current : previous && { ...previous, disabled: current.disabled };
    return source ? [{ row, source }] : [];
  });
  try {
    await queueSourceSave(container, () => ({ ...settings, sources: entries.map(({ source }) => source) }), entries);
  } catch (err) {
    toast(`Sources not saved: ${err.message}`);
  }
}
function sourceRow(source = { type: "addon", name: "", manifestURL: "" }, container = $("sources")) {
  const builtin = sourceTypes.find((type) => type.type === source.type);
  const row = document.createElement("div");
  row.className = "source";
  row.setAttribute("role", "group");
  row.dataset.type = source.type;
  row.savedSource = builtin || source.manifestURL ? { ...source } : null;
  row.dataset.validatedURL = source.manifestURL || "";
  const details = document.createElement("div");
  details.className = "source-details";
  const icon = document.createElement("img");
  icon.className = "source-icon";
  icon.alt = "";
  icon.hidden = true;
  const fallback = document.createElement("span");
  fallback.className = "source-icon source-icon-fallback";
  fallback.setAttribute("aria-hidden", "true");
  const name = document.createElement("strong");
  name.className = "source-name";
  const identity = document.createElement("div");
  identity.className = "source-identity";
  const kind = document.createElement("span");
  kind.className = "source-kind";
  kind.textContent = builtin ? "Built-in" : "Addon";
  identity.append(name, kind);
  details.append(icon, fallback, identity);
  row.append(details);
  const label = document.createElement("label");
  label.className = "source-url";
  label.textContent = builtin?.urlLabel || "Manifest URL";
  const input = document.createElement("input");
  input.type = "url";
  input.dataset.sourceUrl = "";
  input.autocomplete = "url";
  input.required = true;
  input.value = builtin ? source.baseURL : source.manifestURL;
  const status = document.createElement("p");
  status.className = "source-status help";
  status.setAttribute("role", "status");
  status.hidden = true;
  let debounce;
  function setLoading(loading) {
    row.classList.toggle("source-loading", loading);
    row.setAttribute("aria-busy", String(loading));
    fallback.textContent = loading ? "" : name.textContent.charAt(0).toUpperCase();
    if (loading) {
      icon.hidden = true;
      fallback.hidden = false;
    }
  }
  function showDetails(result) {
    row.dataset.name = result.name;
    row.setAttribute("aria-label", result.name);
    name.textContent = result.name;
    setLoading(false);
    fallback.hidden = !!result.icon;
    icon.hidden = !result.icon;
    if (result.icon && icon.dataset.url !== result.icon) {
      icon.dataset.url = result.icon;
      icon.src = result.icon;
    }
    status.hidden = true;
  }
  icon.onerror = () => { icon.hidden = true; fallback.hidden = false; };
  async function fetchDetails(save = false) {
    if (builtin || !completeAddonURL(input)) return;
    const url = input.value.trim();
    const cached = addonDetailsCache.get(url);
    if (save && cached && row.dataset.validatedURL === url && row.savedSource?.manifestURL === url) return;
    status.hidden = true;
    if (!cached) setLoading(true);
    try {
      const result = cached || await lookupAddonDetails(url);
      if (input.value.trim() === url && row.isConnected) {
        showDetails(result);
        row.dataset.validatedURL = url;
        if (save) await saveSources(container);
      }
    } catch (err) {
      if (input.value.trim() === url && row.isConnected) {
        setLoading(false);
        status.textContent = err.message;
        status.hidden = false;
      }
    }
  }
  input.addEventListener("input", () => {
    clearTimeout(debounce);
    status.hidden = true;
    if (!builtin) {
      const url = input.value.trim();
      const cached = addonDetailsCache.get(url);
      showDetails(cached || { name: "Addon source", icon: "" });
      if (!cached) row.dataset.name = "";
      row.dataset.validatedURL = cached ? url : "";
      if (completeAddonURL(input)) {
        if (!cached) setLoading(true);
        debounce = setTimeout(() => fetchDetails(true), 450);
      } else if (input.value.trim()) {
        status.textContent = "Enter an HTTP(S) manifest URL ending in /manifest.json.";
        status.hidden = false;
      }
    }
    syncSourceControls(container);
  });
  input.addEventListener("change", () => {
    clearTimeout(debounce);
    if (builtin) {
      if (completeSourceURL(row)) saveSources(container);
      else toast("Source URL not saved: enter a valid HTTP(S) URL.");
    } else fetchDetails(true);
    syncSourceControls(container);
  });
  label.append(input);
  row.append(label, status);
  const controls = document.createElement("div");
  controls.className = "source-controls";
  const toggleLabel = document.createElement("label");
  toggleLabel.className = "source-toggle";
  const toggle = document.createElement("input");
  toggle.type = "checkbox";
  toggle.setAttribute("role", "switch");
  toggle.setAttribute("aria-label", "Use this source for video");
  toggle.title = "Include this source's video streams in your player";
  toggle.checked = !source.disabled;
  toggle.className = "source-enabled";
  row.classList.toggle("source-disabled", !toggle.checked);
  toggle.onchange = () => {
    row.classList.toggle("source-disabled", !toggle.checked);
    saveSources(container);
  };
  toggleLabel.append(toggle);
  controls.append(toggleLabel);
  const actions = document.createElement("div");
  actions.className = "source-actions";
  const order = document.createElement("div");
  order.className = "source-order";
  order.setAttribute("role", "group");
  order.setAttribute("aria-label", "Source order");
  for (const [direction, symbol] of [["up", "↑"], ["down", "↓"]]) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "text-button source-" + direction;
    button.textContent = symbol;
    button.title = "Move source " + direction;
    button.setAttribute("aria-label", button.title);
    button.onclick = () => {
      const sibling = direction === "up" ? row.previousElementSibling : row.nextElementSibling;
      if (!sibling) return;
      if (direction === "up") container.insertBefore(row, sibling);
      else container.insertBefore(sibling, row);
      syncSourceControls(container);
      saveSources(container);
    };
    order.append(button);
  }
  actions.append(order);
  if (!builtin) {
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "text-button source-remove";
    remove.textContent = "×";
    remove.title = "Remove source";
    remove.setAttribute("aria-label", "Remove source");
    remove.onclick = () => {
      clearTimeout(debounce);
      row.remove();
      syncSourceControls(container);
      saveSources(container);
    };
    actions.append(remove);
  }
  controls.append(actions);
  row.append(controls);
  container.append(row);
  syncSourceControls(container);
  const cached = !builtin && addonDetailsCache.get(source.manifestURL);
  showDetails(cached || { name: builtin?.name || source.name || "Addon source", icon: builtin?.icon || "" });
  if (!builtin && !source.name) row.dataset.name = "";
  if (!builtin && source.manifestURL) fetchDetails();
}
const readSource = (row) => ({
  type: row.dataset.type,
  name: row.dataset.name || "",
  [row.dataset.type === "addon" ? "manifestURL" : "baseURL"]: row.querySelector("[data-source-url]").value.trim(),
  disabled: !row.querySelector(".source-enabled").checked,
});
const readSources = (container) => [...container.children].map(readSource);
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
    const italian = session.listed !== false && session.italian !== false && !session.passthrough;
    const format = !streamFilters.hls && !streamFilters.mkv && !streamFilters.mp4 || !!streamFilters[session.sourceFormat];
    const source = !sourceFilters.size || sourceFilters.has(session.sourceID);
    const matches = (!streamFilters.italian || italian) && format && source;
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
function updateSourceFilterButtons() {
  $("filter-all-sources").setAttribute("aria-pressed", String(!sourceFilters.size));
  for (const [id, button] of sourceFilterButtons) {
    button.setAttribute("aria-pressed", String(sourceFilters.has(id)));
  }
}
function setSourceFilters(next) {
  sourceFilters.clear();
  for (const id of next) {
    if (typeof id === "string" && id) sourceFilters.add(id);
  }
  updateSourceFilterButtons();
  try { localStorage.setItem(sourceFilterKey, JSON.stringify([...sourceFilters])); }
  catch { /* The filters still work for this page session. */ }
  if (latestStatus) applyStreamFilters(latestStatus.sessions);
}
function renderSourceFilters(sources, sessions) {
  const available = new Map(sources.map((source) => [source.id, source.name]));
  for (const session of sessions) {
    if (session.sourceID && !available.has(session.sourceID)) {
      available.set(session.sourceID, session.sourceName || "Addon source");
    }
  }
  for (const [id, button] of sourceFilterButtons) {
    if (!available.has(id)) {
      button.remove();
      sourceFilterButtons.delete(id);
    }
  }
  const container = $("source-filter-options");
  let index = 0;
  for (const [id, name] of available) {
    let button = sourceFilterButtons.get(id);
    if (!button) {
      button = document.createElement("button");
      button.type = "button";
      button.onclick = () => {
        const next = new Set(sourceFilters);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        setSourceFilters(next);
      };
      sourceFilterButtons.set(id, button);
    }
    setText(button, name);
    if (container.children[index] !== button) {
      container.insertBefore(button, container.children[index] || null);
    }
    index++;
  }
  const selected = [...sourceFilters].filter((id) => available.has(id));
  if (selected.length !== sourceFilters.size) setSourceFilters(selected);
  else updateSourceFilterButtons();
}
try {
  const stored = JSON.parse(localStorage.getItem(streamFilterKey) || "{}");
  setStreamFilters(stored && typeof stored === "object" ? stored : {});
} catch { setStreamFilters({}); }
try {
  const stored = JSON.parse(localStorage.getItem(sourceFilterKey) || "[]");
  setSourceFilters(Array.isArray(stored) ? stored : []);
} catch { setSourceFilters([]); }
for (const key of Object.keys(streamFilters)) {
  $("filter-" + key).onclick = () =>
    setStreamFilters({ ...streamFilters, [key]: !streamFilters[key] });
}
$("filter-all-sources").onclick = () => setSourceFilters([]);
$("clear-filters").onclick = () => {
  setStreamFilters({});
  setSourceFilters([]);
};
$("redirect-original").onchange = async () => {
  const toggle = $("redirect-original");
  const enabled = toggle.checked;
  toggle.disabled = true;
  try {
    const result = await api("/api/dev/redirect-original", { enabled });
    toggle.checked = result.enabled;
    toast(result.enabled ? "Original upstream redirects enabled" : "DubLift playback enabled");
  } catch (err) {
    toggle.checked = !enabled;
    toast(err.message);
  } finally { toggle.disabled = false; }
};
const fields = {
  publicURL: "public-url",
  listen: "listen",
  searchRadius: "search-radius",
  alignmentSampleSeconds: "alignment-sample-seconds",
  alignmentSamples: "alignment-samples",
  cacheMB: "cache-mb",
  ffmpeg: "ffmpeg",
  ffprobe: "ffprobe",
  tmdbToken: "tmdb-token",
};
async function loadSettings() {
  [settings, sourceTypes] = await Promise.all([api("/api/settings"), api("/api/source-types")]);
  for (const [id, option] of Object.entries({
    listen: "appListenManaged",
    ffmpeg: "appFFmpegManaged",
    ffprobe: "appFFprobeManaged",
  })) {
    const managed = Boolean(settings[option]);
    const input = $(id);
    $("app-" + id + "-setting").hidden = managed;
    input.required = !managed;
    input.disabled = managed;
  }
  for (const [key, id] of Object.entries(fields))
    $(id).value = settings[key] ?? "";
  $("confidence").value = settings.minConfidence;
  confidenceLabel();
  for (const container of [$("sources"), $("wizard-sources")]) {
    renderSources(container, settings.sources);
  }
}
function confidenceLabel() {
  $("confidence-value").textContent =
    Math.round(Number($("confidence").value) * 100) + "%";
}
$("confidence").oninput = confidenceLabel;
for (const [buttonID, containerID] of [["add-source", "sources"], ["wizard-add-source", "wizard-sources"]]) {
  $(buttonID).onclick = () => {
    sourceRow(undefined, $(containerID));
  };
}
async function saveConfig(cfg, container = $("sources")) {
  const entries = cfg.sources ? [...container.children].map((row) => ({ row })) : [];
  await queueSourceSave(container, () => ({ ...settings, ...cfg }), entries, true);
}
$("settings-form").onsubmit = async (e) => {
  e.preventDefault();
  await waitSettingsWrites();
  const cfg = { ...settings };
  for (const [key, id] of Object.entries(fields))
    cfg[key] = ["searchRadius", "alignmentSampleSeconds", "alignmentSamples", "cacheMB"].includes(key)
      ? Number($(id).value)
      : $(id).value.trim();
  cfg.minConfidence = Number($("confidence").value);
  cfg.sources = readSources($("sources"));
  try {
    await saveConfig(cfg);
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
  $("wizard-progress").textContent = `Step ${setupStep + 1} of 2`;
  for (const section of document.querySelectorAll(".wizard-step"))
    section.hidden = Number(section.dataset.step) !== setupStep;
  for (const [index, label] of [...document.querySelectorAll(".wizard-steps span")].entries())
    label.classList.toggle("active", index === setupStep);
  $("wizard-back").hidden = setupStep === 0;
  $("wizard-next").textContent = setupStep === 1 ? "Finish setup" : "Continue";
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
    await queueSettingsWrite(async () => {
      await api("/api/setup-complete", {});
      settings.setupCompleted = true;
    });
    setupStep = -1;
    renderWizard();
    showView("streams");
    toast("Setup complete. Adjust playback defaults or run setup again in Settings.");
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
      await waitSettingsWrites();
      const invalid = [...$("wizard-sources").children].find((row) => !completeSourceURL(row));
      if (invalid) {
        $("wizard-error").textContent = "Enter a valid HTTP(S) source URL. Addon manifest URLs must end in /manifest.json.";
        invalid.querySelector("[data-source-url]").focus();
        return;
      }
      await saveConfig({ sources: readSources($("wizard-sources")) }, $("wizard-sources"));
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
  $("resolve-status").textContent = "Querying configured sources…";
  try {
    const type = $("content-type").value;
    const id = $("content-id").value.trim();
    const preset = $("preset").selectedOptions[0];
    const [presetType, presetID] = (preset?.value || "").split(":");
    const baseID = id.replace(/^tmdb:/, "").split(":")[0];
    const r = await api("/api/resolve", {
      type,
      id,
      contentName: type === presetType && baseID === presetID ? preset.textContent.trim() : "",
    });
    const hint = !r.streams.length
      ? "Check the activity log for provider errors."
      : "Prepare or play any session to try it.";
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
  card.querySelector(".copy-original").onclick = () => copy(card.dataset.originalUrl);
  const action = (selector, route, body = {}, message = "Request accepted") =>
    (card.querySelector(selector).onclick = async () => {
      const btn = card.querySelector(selector);
      btn.dataset.pending = "true";
      btn.disabled = true;
      btn.setAttribute("aria-busy", "true");
      try {
        const result = await api(`/api/sessions/${id}/${route}`, body);
        toast(typeof message === "function" ? message(result) : message);
      } catch (err) {
        toast(err.message);
      } finally {
        delete btn.dataset.pending;
        btn.removeAttribute("aria-busy");
        const current = latestStatus?.sessions.find((v) => v.id === id);
        if (current) updateCard(current);
        else btn.disabled = false;
      }
    });
  action(".prepare", "prepare", {}, (result) => result.aligned
    ? "Playback prepared with synchronization"
    : result.retryAlignment
      ? "Playback prepared with offset 0. Alignment will retry when playback starts."
      : "Playback prepared with offset 0. An English reference is missing; manual synchronization is available.");
  action(
    ".realign",
    "realign",
    {},
    "Checking audio timing at the latest requested video position",
  );
  action(".analyze", "analyze", {}, "Automatic audio timing check started");
  action(
    ".automatic",
    "offset",
    { offset: null },
    "Manual delay removed. Using saved automatic timing; seek or reopen playback to hear the change.",
  );
  action(".reset", "reset", {}, "Saved timing cleared. A fresh audio check was requested.");
  card.querySelector(".offset-form").onsubmit = async (e) => {
    e.preventDefault();
    const input = card.querySelector(".manual");
    if (input.value === "") return;
    const button = card.querySelector(".apply-offset");
    button.dataset.pending = "true";
    button.disabled = true;
    button.setAttribute("aria-busy", "true");
    try {
      await api(`/api/sessions/${id}/offset`, { offset: Number(input.value) });
      toast("Audio delay updated. Seek or reopen playback to hear the change.");
    } catch (err) {
      toast(err.message);
    } finally {
      delete button.dataset.pending;
      button.disabled = false;
      button.removeAttribute("aria-busy");
    }
  };
  $(inPlaybackSection(session) ? "playing-sessions" : "sessions").append(card);
  cards.set(id, card);
  return card;
}
const normalizedLine = (text) => String(text || "").trim().replace(/\s+/g, " ").toLowerCase();
const normalizedTitleLine = (text) => normalizedLine(String(text || "")
  .trim().replace(/^(?:\p{Extended_Pictographic}|\p{Emoji_Presentation}|\uFE0F|\u200D|[\u{1F3FB}-\u{1F3FF}]|\s)+/u, "")
  .replace(/\s+\((?:18|19|20|21)\d{2}\)$/, ""));
function streamText(v, heading) {
  // Recognize title-only lines with an addon icon or release year. Keep
  // technical details and unfamiliar formatting, even when they include a title.
  const headings = new Set([normalizedTitleLine(heading)]);
  const seen = new Set();
  // Metadata adds this episode suffix to series headings. Also recognize
  // the same series name without that suffix as a duplicate whole line.
  if (v.contentType === "series") headings.add(normalizedTitleLine(String(heading).replace(/ · S\d+E\d+$/, "")));
  return [v.name || "Upstream stream", v.title, v.description].map((text) =>
    String(text || "").split(/\r?\n/).filter((line) => {
      const key = normalizedLine(line);
      if (!key || seen.has(key) || headings.has(normalizedTitleLine(line))) return false;
      seen.add(key);
      return true;
    }).join("\n"),
  );
}
function updateContentLink(card, v) {
  const el = card.querySelector(".content");
  const key = `${v.contentType}:${v.content}`;
  if (el.dataset.value === key) return;
  el.dataset.value = key;
  el.replaceChildren();
  const id = String(v.content || "").replace(/^tmdb:/, "").split(":")[0];
  const imdb = /^tt\d+$/.test(id);
  const tmdb = /^\d+$/.test(id) && ["movie", "series"].includes(v.contentType);
  if (!imdb && !tmdb) {
    el.textContent = v.content || "";
    return;
  }
  const link = document.createElement("a");
  link.className = "inline-link";
  link.href = imdb ? `https://www.imdb.com/title/${id}/` :
    `https://www.themoviedb.org/${v.contentType === "series" ? "tv" : "movie"}/${id}`;
  link.textContent = `${imdb ? "IMDb" : "TMDB"} ${id} ↗`;
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  link.setAttribute("aria-label", `View ${id} on ${imdb ? "IMDb" : "TMDB"} (opens in a new tab)`);
  el.append(link);
}
function sessionMessages(v) {
  let status = v.status || "Waiting for playback";
  let progress = "";
  if (v.passthrough) {
    status = "Original stream · Italian audio was not added";
    progress = `${v.fallbackReason || "DubLift unavailable"}. Playback through the original link cannot be tracked here.`;
  } else if (v.native) {
    if (v.ready) status = "Ready · original source audio and video";
    progress = "Source audio and subtitles use their original timing.";
  } else if (v.aligning) {
    progress = `Sample ${v.sampleIndex || 1} of ${v.sampleTotal || settings?.alignmentSamples || 1} · ${v.samplePhase || "Checking audio timing"}`;
  } else if (v.ready) {
    const a = v.alignment || {};
    const minimum = settings?.minConfidence ?? 0.68;
    const matched = a.confidence > 0 && a.confidence >= minimum;
    const savedAtPosition = (a.boundaries || []).some((b) => b.sourceTime <= v.position && b.confidence >= minimum);
    if (a.manual != null) status = "Ready · manual audio delay";
    else if (matched || savedAtPosition) {
      status = "Ready · audio timing matched";
      if (a.differentEdit) progress = "Different editions detected. Using the earliest reliable audio match.";
    } else {
      status = "Ready · automatic audio timing unavailable";
      progress = v.samplePhase || "";
    }
  } else if (v.samplePhase && !/^Alignment complete ·|^Analysis complete ·/.test(v.samplePhase)) {
    status = v.samplePhase;
  }
  if (v.playing && !v.passthrough) status = `Playback detected · ${status}`;
  const seen = new Set([normalizedLine(status), normalizedLine(progress)].filter(Boolean));
  const errors = (v.errors || []).filter((message) => {
    const key = normalizedLine(message);
    if (!key || seen.has(key) || normalizedLine(status).includes(key) || normalizedLine(progress).includes(key)) return false;
    seen.add(key);
    return true;
  });
  return { status, progress: normalizedLine(status).includes(normalizedLine(progress)) ? "" : progress, errors };
}
function updateCard(v) {
  const card = cards.get(v.id) || createCard(v);
  card.dataset.url = v.url;
  card.dataset.originalUrl = v.originalUrl || "";
  const set = (sel, value) => setText(card.querySelector(sel), value);
  updateContentLink(card, v);
  const format = ["hls", "mkv", "mp4"].includes(v.sourceFormat) ? v.sourceFormat.toUpperCase() : "";
  set(".content-kind", [v.contentType === "series" ? "Series" : v.contentType === "movie" ? "Movie" : "Stream", format].filter(Boolean).join(" · "));
  const heading = v.contentName || v.content;
  set(".content-name", heading);
  const [name, title, description] = streamText(v, heading);
  set(".name", name);
  set(".stream-title", title);
  set(".description", description);
  set(".file-details", v.filename || "");
  card.classList.toggle("playing", !!v.playing);
  const messages = sessionMessages(v);
  set(".status", messages.status);
  set(".sample-progress", messages.progress);
  const positionKnown = v.positionAt && !v.positionAt.startsWith("0001-");
  set(".position", `${positionKnown ? clock(v.position) : "—"} / ${clock(v.duration)}`);
  const a = v.alignment || {};
  const minimum = settings?.minConfidence ?? 0.68;
  let confidence = a.confidence || 0;
  let offset = a.manual ?? (a.confidence >= minimum ? a.offset : 0) ?? 0;
  if (a.manual == null) {
    for (const b of a.boundaries || []) {
      if (b.sourceTime <= v.position && b.confidence >= minimum) {
        offset = b.offset;
        confidence = b.confidence;
      }
    }
  }
  set(".confidence", confidence ? Math.round(confidence * 100) + "%" : "Not matched");
  set(
    ".offset",
    `${offset >= 0 ? "+" : ""}${Number(offset).toFixed(3)} s${a.manual != null ? " · manual" : ""}`,
  );
  set(".tracks", (v.tracks || []).map((t) => t.name).join(" · "));
  card.querySelector(".prepare").textContent = "Prepare playback";
  card.querySelector(".prepare").hidden = !!v.preparationStarted || !!v.ready || !!v.passthrough;
  const disable = (selector, disabled) => {
    const button = card.querySelector(selector);
    button.disabled = !!disabled || button.dataset.pending === "true";
  };
  disable(".prepare", v.listed === false);
  card.querySelector(".session-metrics").hidden = !v.ready || !!v.native;
  card.querySelector(".sync-controls").hidden = !v.ready || !!v.native;
  card.querySelector(".copy").textContent = v.passthrough ? "Copy original upstream URL" : "Copy DubLift URL";
  card.querySelector(".copy-original").hidden = v.passthrough || !v.originalUrl;
  disable(".realign", v.aligning || !v.ready || !positionKnown);
  disable(".analyze", v.aligning || !v.ready);
  disable(".reset", v.aligning || !v.ready);
  disable(".automatic", a.manual == null || !v.ready);
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
          `Upstream ${clock(x.sourceTime)} ↔ Vixsrc ${clock(x.vixTime)} · ${x.offset >= 0 ? "+" : ""}${x.offset.toFixed(3)} s · ${Math.round(x.confidence * 100)}%${x.confidence < minimum ? " · below required confidence" : ""}`,
      ),
      ...(a.boundaries || []).map(
        (x) =>
          `Saved boundary from ${clock(x.sourceTime)} · ${x.offset.toFixed(3)} s`,
      ),
    ]
      .filter(Boolean)
      .join("\n"),
  );
  card.querySelector(".file-info").hidden = !v.filename;
  card.querySelector(".track-info").hidden = !v.tracks?.length;
  card.querySelector(".alignment-info").hidden = !card.querySelector(".anchors").textContent;
  card.querySelector(".technical-details").hidden = !v.filename && !v.tracks?.length && !card.querySelector(".anchors").textContent;
  const errors = card.querySelector(".errors");
  const errorKey = JSON.stringify(messages.errors);
  if (errors.dataset.value !== errorKey) {
    errors.dataset.value = errorKey;
    errors.replaceChildren();
    for (const text of messages.errors) {
      const li = document.createElement("li");
      li.textContent = text;
      errors.append(li);
    }
  }
}
let lastEvents = "";
let videoSample = null;
function updateVideoDownload() {
    const playingVideo = latestStatus?.sessions.find((v) => v.playing);
    const videoStat = $("video-download");
    if (!playingVideo || $("connection-dot").classList.contains("offline")) {
      videoSample = null;
      setText(videoStat, "—");
    } else {
      const now = performance.now();
      const bytes = playingVideo.videoBytes || 0;
      if (videoSample?.id === playingVideo.id && bytes >= videoSample.bytes) {
        const seconds = (now - videoSample.at) / 1000;
        if (seconds >= 0.5) {
          const mbps = (bytes - videoSample.bytes) * 8 / seconds / 1000000;
          setText(videoStat, mbps > 0 ? `${mbps.toFixed(mbps < 10 ? 2 : 1)} Mbps` : playingVideo.videoActive ? "0.00 Mbps" : "Idle");
          videoSample = { id: playingVideo.id, bytes, at: now };
        }
      } else {
        setText(videoStat, "Waiting");
        videoSample = { id: playingVideo.id, bytes, at: now };
      }
    }
}
setInterval(updateVideoDownload, 1000);
function renderStatus(state) {
    latestStatus = state;
    if (!$("redirect-original").disabled) $("redirect-original").checked = !!state.redirectOriginal;
    setText($("connection"), "Connected to local server");
    $("connection-dot").classList.remove("offline");
    if ($("manifest").value !== state.manifestURL) $("manifest").value = state.manifestURL;
    $("install").href = state.manifestURL.replace(/^https?:\/\//, "stremio://");
    setText($("session-count"), `${state.sessions.filter(v => v.playing).length} / ${state.sessions.length}`);
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
    renderSourceFilters(state.sources || [], state.sessions);
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
    updateVideoDownload();
};
