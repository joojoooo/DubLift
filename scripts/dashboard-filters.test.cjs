const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const app = fs.readFileSync(path.join(__dirname, '../internal/dublift/web/app.js'), 'utf8');
const filterCode = app.slice(0, app.indexOf('$("redirect-original").onchange'));
const streamKey = 'dublift.streamFilters.v1';
const sourceKey = 'dublift.sourceFilters.v1';
const sources = [{ id: 'vix', name: 'VixSrc' }, { id: 'first', name: 'Same addon' }, { id: 'second', name: 'Same addon' }];

class Element {
  constructor() { this.children = []; this.textContent = ''; this.hidden = false; }
  setAttribute(name, value) { this[name] = value; }
  insertBefore(node, sibling) {
    node.remove();
    const index = sibling ? this.children.indexOf(sibling) : this.children.length;
    this.children.splice(index, 0, node);
    node.parentElement = this;
  }
  remove() {
    if (!this.parentElement) return;
    const children = this.parentElement.children;
    children.splice(children.indexOf(this), 1);
    this.parentElement = null;
  }
}

function fixture(saved = {}, storageUnavailable = false) {
  const ids = new Map();
  for (const id of ['filter-italian', 'filter-hls', 'filter-mkv', 'filter-mp4', 'filter-all-sources', 'source-filter-options', 'clear-filters', 'empty', 'no-results', 'filter-empty', 'filter-count']) {
    ids.set(id, new Element());
  }
  const storage = new Map(Object.entries(saved));
  const context = vm.createContext({
    document: { getElementById: id => ids.get(id), createElement: () => new Element() },
    localStorage: {
      getItem(key) { if (storageUnavailable) throw new Error('Storage unavailable'); return storage.get(key); },
      setItem(key, value) { if (storageUnavailable) throw new Error('Storage unavailable'); storage.set(key, value); },
    },
  });
  vm.runInContext(filterCode, context);
  return {
    id: id => ids.get(id), storage,
    render(available, sessions) {
      context.state = { sources: available, sessions };
      vm.runInContext(`
        latestStatus = state;
        for (const session of state.sessions) {
          if (!cards.has(session.id)) cards.set(session.id, document.createElement('article'));
        }
        renderSourceFilters(state.sources, state.sessions);
        applyStreamFilters(state.sessions);
      `, context);
    },
    visible(sessions) {
      context.sessions = sessions;
      return [...vm.runInContext('sessions.filter(session => !cards.get(session.id).hidden).map(session => session.id)', context)];
    },
    source(id) { context.sourceID = id; return vm.runInContext('sourceFilterButtons.get(sourceID)', context); },
  };
}

const session = (id, sourceID, sourceFormat, extra = {}) => ({ id, sourceID, sourceFormat, ...extra });
const results = [
  session('vix-hls', 'vix', 'hls'),
  session('first-hls', 'first', 'hls', { passthrough: true }),
  session('first-mkv', 'first', 'mkv'),
  session('second-mp4', 'second', 'mp4'),
  session('second-external', 'second', '', { passthrough: true }),
  session('playing', 'second', 'mp4', { playing: true }),
  session('preparing', 'second', 'mkv', { preparationStarted: true }),
];

test('source choices include configured addons without results and distinguish equal names', () => {
  const h = fixture();
  h.render(sources, []);
  assert.deepEqual(h.id('source-filter-options').children.map(button => button.textContent), ['VixSrc', 'Same addon', 'Same addon']);
  assert.notEqual(h.source('first'), h.source('second'));
  assert.equal(h.id('filter-all-sources')['aria-pressed'], 'true');
  assert.equal(h.id('empty').hidden, false);
  assert.equal(h.id('filter-count').textContent, '');
});

test('multiple sources and types combine with Italian while playback stays visible', () => {
  const h = fixture();
  h.render(sources, results);
  h.source('first').onclick();
  assert.deepEqual(h.visible(results), ['first-hls', 'first-mkv', 'playing', 'preparing']);
  assert.equal(h.id('filter-count').textContent, 'Showing 2 of 5');
  h.source('second').onclick();
  h.id('filter-hls').onclick();
  h.id('filter-mkv').onclick();
  h.id('filter-italian').onclick();
  assert.deepEqual(h.visible(results), ['first-mkv', 'playing', 'preparing']);
  assert.equal(h.id('filter-count').textContent, 'Showing 1 of 5');
  assert.equal(h.id('filter-all-sources')['aria-pressed'], 'false');
  h.source('first').onclick();
  assert.deepEqual(h.visible(results), ['playing', 'preparing']);
  assert.equal(h.id('filter-empty').hidden, false);
  h.id('clear-filters').onclick();
  assert.deepEqual(h.visible(results), results.map(value => value.id));
  assert.equal(h.id('filter-empty').hidden, true);
  assert.equal(h.id('filter-italian')['aria-pressed'], 'false');
  assert.equal(h.source('second')['aria-pressed'], 'false');
});

test('All clears only sources and toggling the final source restores all', () => {
  const h = fixture();
  h.render(sources, results);
  h.id('filter-hls').onclick();
  h.source('first').onclick();
  h.id('filter-all-sources').onclick();
  assert.deepEqual(h.visible(results), ['vix-hls', 'first-hls', 'playing', 'preparing']);
  assert.equal(h.id('filter-hls')['aria-pressed'], 'true');
  h.source('second').onclick();
  h.source('second').onclick();
  assert.deepEqual(h.visible(results), ['vix-hls', 'first-hls', 'playing', 'preparing']);
  assert.equal(h.id('filter-all-sources')['aria-pressed'], 'true');
});

test('saved sources restore before results arrive and remain separate from stream preferences', () => {
  const h = fixture({ [sourceKey]: '["first"]', [streamKey]: '{"mkv":true}' });
  h.render(sources, []);
  assert.equal(h.source('first')['aria-pressed'], 'true');
  h.render(sources, results);
  assert.deepEqual(h.visible(results), ['first-mkv', 'playing', 'preparing']);
  assert.equal(h.storage.get(sourceKey), '["first"]');
  assert.equal(JSON.parse(h.storage.get(streamKey)).mkv, true);
});

test('refresh preserves buttons and selections across reordering, renaming, and retained sessions', () => {
  const h = fixture();
  h.render(sources, results);
  const firstButton = h.source('first');
  firstButton.onclick();
  h.render([sources[2], sources[0], { id: 'first', name: 'Renamed' }], results);
  assert.equal(h.source('first'), firstButton);
  assert.equal(firstButton.textContent, 'Renamed');
  assert.equal(firstButton['aria-pressed'], 'true');
  assert.equal(h.id('source-filter-options').children[2], firstButton);
  h.render([sources[0]], [session('retained', 'first', 'mkv', { sourceName: 'Old addon', playing: true })]);
  assert.equal(h.source('first'), firstButton);
  assert.equal(firstButton.textContent, 'Old addon');
  assert.equal(firstButton['aria-pressed'], 'true');
  h.render([sources[0]], [results[0]]);
  assert.equal(h.source('first'), undefined);
  assert.equal(h.id('filter-all-sources')['aria-pressed'], 'true');
  assert.deepEqual(h.visible([results[0]]), ['vix-hls']);
  assert.equal(h.storage.get(sourceKey), '[]');
});

test('invalid saved preferences and unavailable browser storage leave filters usable', () => {
  for (const saved of ['{', '{}', 'null', '[null,1,""]']) {
    const h = fixture({ [sourceKey]: saved });
    h.render(sources, results);
    assert.deepEqual(h.visible(results), results.map(value => value.id));
  }
  const h = fixture({}, true);
  h.render(sources, results);
  h.source('vix').onclick();
  assert.deepEqual(h.visible(results), ['vix-hls', 'playing', 'preparing']);
});
