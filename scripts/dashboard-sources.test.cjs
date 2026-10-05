const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const app = fs.readFileSync(path.join(__dirname, '../internal/dublift/web/app.js'), 'utf8');
// Exercise the production source-card handlers and save queue in a small DOM
// fixture. This needs no browser, third-party modules, or live provider access.
const sourceCode = app.slice(0, app.indexOf('const inPlaybackSection'));
const configCode = app.slice(app.indexOf('async function saveConfig('), app.indexOf('$("settings-form").onsubmit'));
const addCode = app.slice(app.indexOf('for (const [buttonID, containerID]'), app.indexOf('async function saveConfig('));
const finishCode = app.slice(app.indexOf('async function finishWizard('), app.indexOf('$("wizard-next").onclick'));
const nextCode = app.slice(app.indexOf('$("wizard-next").onclick'), app.indexOf('$("preset").onchange'));
const clone = value => JSON.parse(JSON.stringify(value));
const vix = { type: 'vixsrc', name: 'VixSrc', baseURL: 'https://vix.test', disabled: false };
const addon = name => ({ type: 'addon', name, manifestURL: `https://${name.toLowerCase()}.test/manifest.json`, disabled: false });

class Element {
  constructor(tag) {
    this.tag = tag;
    this.children = [];
    this.dataset = {};
    this.listeners = {};
    this.style = {};
    this.className = '';
    this.value = '';
    this.classList = { toggle: (name, on) => {
      const names = new Set(this.className.split(' ').filter(Boolean));
      if (on) names.add(name); else names.delete(name);
      this.className = [...names].join(' ');
    }};
  }
  append(...nodes) {
    for (const node of nodes) {
      if (!(node instanceof Element)) continue;
      node.remove();
      this.children.push(node);
      node.parentElement = this;
    }
  }
  remove() {
    if (!this.parentElement) return;
    const list = this.parentElement.children;
    list.splice(list.indexOf(this), 1);
    this.parentElement = null;
  }
  replaceChildren() { for (const child of [...this.children]) child.remove(); }
  insertBefore(node, sibling) {
    node.remove();
    this.children.splice(this.children.indexOf(sibling), 0, node);
    node.parentElement = this;
  }
  get previousElementSibling() { return this.parentElement?.children[this.parentElement.children.indexOf(this) - 1]; }
  get nextElementSibling() { return this.parentElement?.children[this.parentElement.children.indexOf(this) + 1]; }
  get isConnected() { return !!this.connected || !!this.parentElement?.isConnected; }
  set src(value) { this.imageURL = value; this.srcAssignments = (this.srcAssignments || 0) + 1; }
  get src() { return this.imageURL; }
  setAttribute(name, value) { this[name] = value; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  querySelector(selector) {
    for (const child of this.children) {
      if (selector === '[data-source-url]' && 'sourceUrl' in child.dataset) return child;
      if (selector.startsWith('.') && child.className.split(' ').includes(selector.slice(1))) return child;
      const found = child.querySelector(selector);
      if (found) return found;
    }
    return null;
  }
  checkValidity() {
    if (!this.value.trim()) return false;
    try { new URL(this.value); return true; } catch { return false; }
  }
}

function fixture(sources = [vix], options = {}) {
  const body = new Element('body'); body.connected = true;
  const ids = new Map();
  for (const id of ['sources', 'wizard-sources', 'add-source', 'wizard-add-source', 'wizard-next', 'wizard-error', 'toast', 'cache-mb']) {
    const el = new Element('div'); ids.set(id, el); body.append(el);
  }
  let stored = { configVersion: 1, cacheMB: 256, minConfidence: 0.68, setupCompleted: false, sources: clone(sources) };
  let gate, manifestGate, failSave = false, rejectManifest = !!options.rejectManifest, active = 0, maxActive = 0;
  const writes = [];
  const manifestRequests = [];
  const context = vm.createContext({
    document: { getElementById: id => ids.get(id), createElement: tag => new Element(tag) },
    URL, Map, Set,
    setTimeout: (fn, ms) => { const timer = setTimeout(fn, ms); timer.unref(); return timer; },
    clearTimeout,
    renderWizard: () => {}, showView: () => {},
    fetch: async (url, options) => {
      const data = JSON.parse(options.body);
      if (url === '/api/addon-name') {
        manifestRequests.push(data.manifestURL);
        if (manifestGate) { const waiting = manifestGate; manifestGate = null; await waiting; }
        const name = new URL(data.manifestURL).hostname.split('.')[0];
        return { ok: !rejectManifest, json: async () => rejectManifest ? { error: 'Manifest unavailable' } : { name: name.charAt(0).toUpperCase() + name.slice(1), icon: new URL('/logo.png', data.manifestURL).href } };
      }
      const completing = url === '/api/setup-complete';
      assert.ok(completing || url === '/api/settings');
      const candidate = completing ? { ...clone(stored), setupCompleted: true } : { ...clone(data), setupCompleted: stored.setupCompleted };
      if (!completing) {
        assert.equal(data.configVersion, 1, 'settings saves must preserve the config version');
        writes.push(clone(data));
      }
      active++; maxActive = Math.max(maxActive, active);
      if (gate) { const waiting = gate; gate = null; await waiting; }
      active--;
      if (failSave) { failSave = false; return { ok: false, json: async () => ({ error: 'Disk write failed' }) }; }
      stored = candidate;
      return { ok: true, json: async () => clone(stored) };
    },
    initial: clone(stored),
  });
  vm.runInContext(sourceCode + addCode + configCode + finishCode + nextCode, context);
  vm.runInContext('settings = initial; sourceTypes = [{ type: "vixsrc", name: "VixSrc", urlLabel: "Vixsrc base URL", icon: "/vixsrc.ico" }]; renderSources($("sources"), settings.sources); renderSources($("wizard-sources"), settings.sources);', context);
  return {
    id: id => ids.get(id),
    run: code => vm.runInContext(code, context),
    stored: () => stored, writes, manifestRequests,
    blockSave: promise => { gate = promise; },
    blockManifest: promise => { manifestGate = promise; },
    failSave: () => { failSave = true; },
    rejectManifest: () => { rejectManifest = true; },
    allowManifest: () => { rejectManifest = false; },
    maxActive: () => maxActive,
    async settle() { await new Promise(setImmediate); await vm.runInContext('settingsWrite', context); await new Promise(setImmediate); },
  };
}
function enterURL(row, value) {
  const input = row.querySelector('[data-source-url]');
  input.value = value;
  input.listeners.input();
  input.listeners.change();
}

test('blur after manifest autosave keeps the loaded logo and avoids another lookup or save', async () => {
  for (const containerID of ['sources', 'wizard-sources']) {
    const h = fixture(); await h.settle();
    h.run(`sourceRow(undefined, $("${containerID}"))`);
    const row = h.id(containerID).children[1];
    const input = row.querySelector('[data-source-url]');
    input.value = 'https://new.test/manifest.json';
    input.listeners.input();
    await new Promise(resolve => setTimeout(resolve, 500)); await h.settle();
    const icon = row.querySelector('.source-icon');
    const assignments = icon.srcAssignments;
    assert.equal(icon.hidden, false);
    assert.deepEqual(h.manifestRequests, [input.value]);
    assert.equal(h.writes.length, 1);
    input.listeners.change();
    assert.equal(row['aria-busy'], 'false', 'blur must not restart the spinner');
    assert.equal(icon.hidden, false, 'blur must keep the logo visible');
    await h.settle();
    assert.deepEqual(h.manifestRequests, [input.value]);
    assert.equal(icon.srcAssignments, assignments, 'blur must not reload the logo');
    assert.equal(h.writes.length, 1);
  }
});

test('source reorders and toggles reuse addon details across settings and setup', async () => {
  for (const containerID of ['sources', 'wizard-sources']) {
    const h = fixture([vix, addon('First'), addon('Second')]); await h.settle();
    assert.deepEqual(h.manifestRequests, [addon('First').manifestURL, addon('Second').manifestURL], 'initial lists share pending lookups');
    const row = h.id(containerID).children[2];
    const icon = row.querySelector('.source-icon');
    const assignments = icon.srcAssignments;
    row.querySelector('.source-up').onclick(); await h.settle();
    row.querySelector('.source-up').onclick(); await h.settle();
    const toggle = row.querySelector('.source-enabled');
    toggle.checked = false; toggle.onchange(); await h.settle();
    assert.equal(h.manifestRequests.length, 2);
    assert.equal(icon.srcAssignments, assignments);
    assert.deepEqual(h.stored().sources.map(s => s.name), ['Second', 'VixSrc', 'First']);
    assert.equal(h.stored().sources[0].disabled, true);
    for (const id of ['sources', 'wizard-sources']) {
      for (const source of h.id(id).children.filter(source => source.dataset.type === 'addon')) {
        assert.equal(source['aria-busy'], 'false');
        assert.equal(source.querySelector('.source-icon').hidden, false);
      }
    }
  }
});

test('addon details stay tied to their URL when an older lookup finishes after an edit', async () => {
  const h = fixture(); await h.settle();
  h.run('sourceRow()');
  const row = h.id('sources').children[1];
  let release;
  h.blockManifest(new Promise(resolve => { release = resolve; }));
  enterURL(row, 'https://first.test/manifest.json');
  enterURL(row, 'https://second.test/manifest.json');
  await h.settle();
  assert.equal(row.querySelector('.source-name').textContent, 'Second');
  release(); await h.settle();
  assert.equal(row.querySelector('.source-name').textContent, 'Second');
  assert.equal(row.querySelector('.source-icon').src, 'https://second.test/logo.png');
  assert.equal(h.stored().sources[1].manifestURL, 'https://second.test/manifest.json');
  enterURL(row, 'https://first.test/manifest.json');
  assert.equal(row['aria-busy'], 'false', 'returning to a loaded URL must not restart the spinner');
  await h.settle();
  assert.equal(row.querySelector('.source-name').textContent, 'First');
  assert.equal(row.querySelector('.source-icon').src, 'https://first.test/logo.png');
  assert.equal(h.stored().sources[1].manifestURL, 'https://first.test/manifest.json');
  assert.equal(h.manifestRequests.length, 2);
});

test('failed manifest lookups retry on blur and reuse details after a successful retry', async () => {
  for (const configured of [false, true]) {
    const h = fixture(configured ? [vix, addon('Retry')] : [vix], { rejectManifest: true });
    await h.settle();
    if (!configured) h.run('sourceRow()');
    const row = h.id('sources').children[1];
    if (!configured) enterURL(row, 'https://retry.test/manifest.json');
    await h.settle();
    assert.equal(row.querySelector('.source-status').textContent, 'Manifest unavailable');
    assert.equal(h.writes.length, 0);
    h.allowManifest();
    row.querySelector('[data-source-url]').listeners.change(); await h.settle();
    assert.equal(row.querySelector('.source-name').textContent, 'Retry');
    assert.equal(row.querySelector('.source-status').hidden, true);
    assert.equal(row['aria-busy'], 'false');
    assert.equal(h.manifestRequests.length, 2);
    row.querySelector('[data-source-url]').listeners.change(); await h.settle();
    assert.equal(h.manifestRequests.length, 2);
    assert.equal(h.writes.length, 1);
  }
});

test('validated addon additions save automatically in settings and setup', async () => {
  for (const containerID of ['sources', 'wizard-sources']) {
    const h = fixture(); await h.settle();
    assert.equal(h.writes.length, 0, 'loading cards must not save');
    h.id('cache-mb').value = '512';
    h.run(`sourceRow(undefined, $("${containerID}"))`);
    const row = h.id(containerID).children.at(-1);
    enterURL(row, 'https://new.test/manifest.json');
    await h.settle();
    assert.equal(h.stored().sources.length, 2);
    assert.equal(h.stored().sources[1].manifestURL, 'https://new.test/manifest.json');
    assert.equal(h.writes.length, 1);
    assert.equal(h.stored().cacheMB, 256);
    assert.equal(h.id('cache-mb').value, '512', 'source autosave must preserve other form edits');
    assert.equal(h.id(containerID === 'sources' ? 'wizard-sources' : 'sources').children.length, 2);
  }
});

test('rapid reorders serialize writes and retain the final order', async () => {
  const h = fixture([vix, addon('First'), addon('Second')]); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  const row = h.id('sources').children[2];
  row.querySelector('.source-up').onclick();
  await new Promise(setImmediate);
  row.querySelector('.source-up').onclick();
  assert.equal(h.writes.length, 1);
  release(); await h.settle();
  assert.deepEqual(h.stored().sources.map(s => s.type === 'vixsrc' ? 'VixSrc' : s.name), ['Second', 'VixSrc', 'First']);
  assert.equal(h.maxActive(), 1);
});

test('adding or editing drafts during autosave still synchronizes the other view before setup saves', async () => {
  for (const containerID of ['sources', 'wizard-sources']) {
    const h = fixture(); await h.settle();
    let release;
    h.blockSave(new Promise(resolve => { release = resolve; }));
    const addID = containerID === 'sources' ? 'add-source' : 'wizard-add-source';
    h.id(addID).onclick();
    const row = h.id(containerID).children[1];
    enterURL(row, 'https://new.test/manifest.json');
    await new Promise(setImmediate);
    h.id(addID).onclick();
    const draft = h.id(containerID).children[2];
    enterURL(draft, 'unfinished addon');
    enterURL(row, 'unfinished existing edit');
    release(); await h.settle();
    const otherID = containerID === 'sources' ? 'wizard-sources' : 'sources';
    assert.equal(h.id(otherID).children.length, 2);
    assert.equal(h.id(otherID).children[1].querySelector('[data-source-url]').value, 'https://new.test/manifest.json');
    await h.run(`saveConfig({ sources: readSources($("${otherID}")) }, $("${otherID}"))`);
    await h.run('finishWizard()'); await h.settle();
    assert.equal(h.stored().sources.length, 2, 'finishing setup must retain the saved addon');
    assert.equal(h.stored().setupCompleted, true);
    assert.equal(row.querySelector('[data-source-url]').value, 'unfinished existing edit');
    assert.equal(draft.querySelector('[data-source-url]').value, 'unfinished addon');
    assert.equal(row.isConnected, true);
    assert.equal(draft.isConnected, true);
  }
});

test('synchronizing the other view retains its drafts and pending manifest validation', async () => {
  const h = fixture([vix, addon('First')]); await h.settle();
  const edited = h.id('wizard-sources').children[1];
  enterURL(edited, 'unfinished existing URL');
  h.id('wizard-add-source').onclick();
  const draft = h.id('wizard-sources').children[2];
  const input = draft.querySelector('[data-source-url]');
  input.value = 'https://pending.test/manifest.json';
  input.listeners.input();
  const toggle = h.id('sources').children[0].querySelector('.source-enabled');
  toggle.checked = false; toggle.onchange();
  await h.settle();
  assert.equal(edited.isConnected, true);
  assert.equal(draft.isConnected, true);
  assert.equal(edited.querySelector('[data-source-url]').value, 'unfinished existing URL');
  assert.equal(h.id('wizard-sources').children[0].querySelector('.source-enabled').checked, false);
  await new Promise(resolve => setTimeout(resolve, 500)); await h.settle();
  assert.equal(h.stored().sources.length, 3);
  assert.equal(h.stored().sources[2].manifestURL, 'https://pending.test/manifest.json');
  assert.equal(h.stored().sources[0].disabled, true);
});

test('source editing in the other view waits for pending reorders to synchronize', async () => {
  const h = fixture([vix, addon('First'), addon('Second')]); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  h.id('sources').children[2].querySelector('.source-up').onclick();
  await new Promise(setImmediate);
  assert.equal(h.id('wizard-add-source').disabled, true);
  for (const row of h.id('wizard-sources').children) {
    for (const selector of ['[data-source-url]', '.source-enabled', '.source-up', '.source-down', '.source-remove']) {
      const control = row.querySelector(selector);
      if (control) assert.equal(control.disabled, true, `${selector} must wait for synchronization`);
    }
  }
  release(); await h.settle();
  assert.equal(h.id('wizard-add-source').disabled, false);
  assert.equal(h.id('wizard-sources').children[2].querySelector('.source-up').disabled, false);
  assert.equal(h.id('wizard-sources').children[2].querySelector('.source-down').disabled, true, 'last source cannot move down');
  const row = h.id('wizard-sources').children[2];
  row.querySelector('.source-up').onclick();
  row.querySelector('.source-up').onclick();
  const toggle = row.querySelector('.source-enabled'); toggle.checked = false; toggle.onchange();
  enterURL(row, 'unfinished URL');
  await h.settle();
  assert.deepEqual(h.stored().sources.map(s => s.type === 'vixsrc' ? 'VixSrc' : s.name), ['First', 'VixSrc', 'Second']);
  assert.equal(h.stored().sources[0].disabled, true);
  assert.equal(row.isConnected, true);
  assert.equal(row.querySelector('[data-source-url]').value, 'unfinished URL');
  assert.equal(h.id('sources').children[0].querySelector('.source-enabled').checked, false);
  assert.equal(h.maxActive(), 1);
});

test('pending addon additions synchronize before the other view can toggle VixSrc', async () => {
  for (const containerID of ['sources', 'wizard-sources']) {
    const h = fixture(); await h.settle();
    const otherID = containerID === 'sources' ? 'wizard-sources' : 'sources';
    let release;
    h.blockSave(new Promise(resolve => { release = resolve; }));
    h.run(`sourceRow(undefined, $("${containerID}"))`);
    enterURL(h.id(containerID).children[1], 'https://new.test/manifest.json');
    await new Promise(setImmediate);
    assert.equal(h.id(otherID).children.length, 1, 'the other list is stale while the save is pending');
    const toggle = h.id(otherID).children[0].querySelector('.source-enabled');
    assert.equal(toggle.disabled, true);
    assert.equal(h.id(containerID).children[0].querySelector('.source-enabled').disabled, false);
    // Even a change already dispatched before disabling must wait to save.
    toggle.checked = false; toggle.onchange();
    await new Promise(setImmediate);
    assert.equal(h.writes.length, 1);
    release(); await h.settle();
    assert.equal(toggle.disabled, false);
    assert.deepEqual(h.stored().sources, [{ ...vix, disabled: true }, addon('New')]);
    assert.equal(h.writes.length, 2);
    assert.equal(h.id(containerID).children.length, 2);
    assert.equal(h.maxActive(), 1);
  }
});

test('manifest validations finishing during another view save use its synchronized list', async () => {
  for (const containerID of ['sources', 'wizard-sources']) {
    for (const remove of [false, true]) {
      const h = fixture([vix, addon('First')]); await h.settle();
      const otherID = containerID === 'sources' ? 'wizard-sources' : 'sources';
      let releaseSave, releaseManifest;
      h.blockManifest(new Promise(resolve => { releaseManifest = resolve; }));
      h.run(`sourceRow(undefined, $("${otherID}"))`);
      enterURL(h.id(otherID).children[2], 'https://pending.test/manifest.json');
      h.blockSave(new Promise(resolve => { releaseSave = resolve; }));
      if (remove) h.id(containerID).children[1].querySelector('.source-remove').onclick();
      else {
        h.run(`sourceRow(undefined, $("${containerID}"))`);
        enterURL(h.id(containerID).children[2], 'https://new.test/manifest.json');
      }
      await new Promise(setImmediate);
      releaseManifest(); await new Promise(setImmediate);
      assert.equal(h.writes.length, 1, 'manifest autosave must wait for the other list');
      releaseSave(); await h.settle();
      assert.deepEqual(h.stored().sources, remove
        ? [vix, addon('Pending')]
        : [vix, addon('First'), addon('Pending'), addon('New')]);
      assert.equal(h.writes.length, 2);
      assert.equal(h.maxActive(), 1);
      assert.equal(h.id(otherID).children[0].querySelector('.source-enabled').disabled, false);
    }
  }
});

test('a failed source save re-enables editing in the other view', async () => {
  const h = fixture(); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  h.failSave();
  const toggle = h.id('sources').children[0].querySelector('.source-enabled');
  toggle.checked = false; toggle.onchange();
  assert.equal(h.id('wizard-add-source').disabled, true);
  assert.equal(h.id('wizard-sources').children[0].querySelector('.source-enabled').disabled, true);
  release(); await h.settle();
  assert.match(h.id('toast').textContent, /Sources not saved: Disk write failed/);
  assert.equal(h.id('wizard-add-source').disabled, false);
  assert.equal(h.id('wizard-sources').children[0].querySelector('.source-enabled').disabled, false);
});

test('synchronizing a draft URL applies saved toggles without overwriting the draft', async () => {
  const h = fixture([vix, addon('First')]); await h.settle();
  const draft = h.id('wizard-sources').children[1];
  enterURL(draft, 'unfinished existing URL');
  const toggle = h.id('sources').children[1].querySelector('.source-enabled');
  toggle.checked = false; toggle.onchange(); await h.settle();
  assert.equal(draft.isConnected, true);
  assert.equal(draft.querySelector('[data-source-url]').value, 'unfinished existing URL');
  assert.equal(draft.querySelector('.source-enabled').checked, false);
  draft.querySelector('.source-up').onclick(); await h.settle();
  assert.equal(h.stored().sources[0].manifestURL, 'https://first.test/manifest.json');
  assert.equal(h.stored().sources[0].disabled, true);
});

test('finishing setup waits for pending autosave before collecting its source list', async () => {
  const h = fixture(); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  h.id('add-source').onclick();
  enterURL(h.id('sources').children[1], 'https://new.test/manifest.json');
  await new Promise(setImmediate);
  h.id('add-source').onclick();
  const draft = h.id('sources').children[2];
  enterURL(draft, 'unfinished URL');
  h.run('setupStep = 1');
  const finishing = h.id('wizard-next').onclick();
  const toggle = h.id('sources').children[1].querySelector('.source-enabled');
  toggle.checked = false; toggle.onchange();
  release(); await finishing; await h.settle();
  assert.equal(h.stored().sources.length, 2);
  assert.equal(h.stored().sources[1].manifestURL, 'https://new.test/manifest.json');
  assert.equal(h.stored().sources[1].disabled, true, 'setup must retain saves queued while it waits');
  assert.equal(h.stored().setupCompleted, true);
  assert.equal(draft.isConnected, true);
  assert.equal(draft.querySelector('[data-source-url]').value, 'unfinished URL');
  assert.equal(h.maxActive(), 1);
});

test('manifest validation saves a pasted URL before it loses focus', async () => {
  const h = fixture(); await h.settle();
  h.run('sourceRow()');
  const input = h.id('sources').children[1].querySelector('[data-source-url]');
  input.value = 'https://paste.test/manifest.json';
  input.listeners.input();
  await new Promise(resolve => setTimeout(resolve, 500));
  await h.settle();
  assert.equal(h.stored().sources[1].manifestURL, 'https://paste.test/manifest.json');
  assert.equal(h.writes.length, 1);
});

test('draft URLs do not block saving order or replace existing source URLs', async () => {
  const h = fixture([vix, addon('First'), addon('Second')]); await h.settle();
  h.run('sourceRow()');
  enterURL(h.id('sources').children[1], 'invalid existing URL');
  enterURL(h.id('sources').children[3], 'incomplete new URL');
  h.id('sources').children[2].querySelector('.source-up').onclick();
  await h.settle();
  assert.equal(h.stored().sources.length, 3);
  assert.equal(h.stored().sources[1].name, 'Second');
  assert.equal(h.stored().sources[2].manifestURL, 'https://first.test/manifest.json');
  assert.equal(h.id('sources').children[3].querySelector('[data-source-url]').value, 'incomplete new URL');
});

test('failed manifests never add a source to saved settings', async () => {
  const h = fixture(); await h.settle(); h.rejectManifest();
  h.run('sourceRow()');
  const row = h.id('sources').children[1];
  enterURL(row, 'https://bad.test/manifest.json'); await h.settle();
  assert.equal(h.writes.length, 0);
  assert.equal(h.stored().sources.length, 1);
  assert.equal(row.querySelector('.source-status').textContent, 'Manifest unavailable');
});

test('editing a URL while its addition is saving does not lose the valid source', async () => {
  const h = fixture(); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  h.run('sourceRow()');
  const row = h.id('sources').children[1];
  enterURL(row, 'https://new.test/manifest.json');
  await new Promise(setImmediate);
  assert.equal(h.writes.length, 1);
  enterURL(row, 'unfinished edit');
  row.querySelector('.source-up').onclick();
  release(); await h.settle();
  assert.equal(h.stored().sources.length, 2);
  assert.equal(h.stored().sources[0].manifestURL, 'https://new.test/manifest.json');
  assert.equal(row.querySelector('[data-source-url]').value, 'unfinished edit');
});

test('failed writes are visible and do not block later toggle or removal saves', async () => {
  const h = fixture([vix, addon('First')]); await h.settle(); h.failSave();
  const row = h.id('sources').children[1];
  row.querySelector('.source-up').onclick(); await h.settle();
  assert.match(h.id('toast').textContent, /Sources not saved: Disk write failed/);
  const toggle = row.querySelector('.source-enabled'); toggle.checked = false; toggle.onchange();
  await h.settle();
  assert.equal(h.stored().sources[0].disabled, true);
  row.querySelector('.source-remove').onclick(); await h.settle();
  assert.equal(h.stored().sources.length, 1);
  assert.equal(h.stored().sources[0].type, 'vixsrc');
});

test('queued source saves preserve a preceding general settings save', async () => {
  const h = fixture([vix, addon('First')]); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  const row = h.id('sources').children[1];
  row.querySelector('.source-up').onclick();
  await new Promise(setImmediate);
  const manual = h.run('saveConfig({ cacheMB: 384, sources: readSources($("sources")) })');
  const toggle = row.querySelector('.source-enabled'); toggle.checked = false; toggle.onchange();
  release(); await manual; await h.settle();
  assert.equal(h.stored().cacheMB, 384);
  assert.equal(h.stored().sources[0].disabled, true);
  assert.equal(h.maxActive(), 1);
});

test('finishing setup and pending source changes cannot overwrite each other', async () => {
  const h = fixture([vix, addon('First')]); await h.settle();
  let release;
  h.blockSave(new Promise(resolve => { release = resolve; }));
  const finishing = h.run('finishWizard()');
  await new Promise(setImmediate);
  const row = h.id('wizard-sources').children[1];
  row.querySelector('.source-up').onclick();
  const toggle = row.querySelector('.source-enabled'); toggle.checked = false; toggle.onchange();
  assert.equal(h.writes.length, 0, 'source writes must wait for setup completion');
  release(); await finishing; await h.settle();
  assert.equal(h.stored().setupCompleted, true);
  assert.equal(h.stored().sources[0].name, 'First');
  assert.equal(h.stored().sources[0].disabled, true);
  assert.equal(h.maxActive(), 1);
});
