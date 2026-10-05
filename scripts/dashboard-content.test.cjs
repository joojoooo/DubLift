const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const app = fs.readFileSync(path.join(__dirname, '../internal/dublift/web/app.js'), 'utf8');
const textCode = app.slice(app.indexOf('const normalizedLine'), app.indexOf('function updateContentLink'));
const resolverCode = app.slice(app.indexOf('$("resolve-form").onsubmit'), app.indexOf('function createCard'));

function streamText(session, heading) {
  const context = vm.createContext({ session, heading });
  vm.runInContext(textCode, context);
  return [...vm.runInContext('streamText(session, heading)', context)];
}

test('Inception appears once with either a canonical or decorated heading', () => {
  const session = {
    contentType: 'movie', name: 'PenguPlay', title: 'Inception',
    description: '🍿 Inception (2010)\r\n📺 1080p\n💾 4 GB',
  };
  for (const heading of ['Inception', '🍿 Inception (2010)']) {
    assert.deepEqual(streamText(session, heading), ['PenguPlay', '', '📺 1080p\n💾 4 GB']);
  }
});

test('VixSrc resolution, bitrate, and codecs stay in the stream information', () => {
  const details = '📺 1280×720 · 📶 1.8 Mbps\n🎞️ avc1.640028 / mp4a.40.2';
  assert.deepEqual(streamText({ name: 'VixSrc · 720p', title: details }, 'Inception'), ['VixSrc · 720p', details, '']);
});

test('duplicate titles recognize case, spacing, icons, and release years without losing other information', () => {
  const session = {
    name: 'Addon',
    title: '🎬  INCEPTION (2010)\nInception · Director’s commentary\nInception (2010) · 1080p',
    description: 'inception\nInception (Extended cut)\n💾 4 GB\n💾 4 GB',
  };
  assert.deepEqual(streamText(session, 'Inception'), [
    'Addon', 'Inception · Director’s commentary\nInception (2010) · 1080p', 'Inception (Extended cut)\n💾 4 GB',
  ]);
  assert.deepEqual(streamText({ name: 'Addon', description: '🍿 [REC] (2007)\n🍿 $5 a Day (2008)' }, '[REC]'), [
    'Addon', '', '🍿 $5 a Day (2008)',
  ]);
});

test('series headings remove repeated series names and preserve episode details', () => {
  const session = {
    contentType: 'series', name: 'Addon', title: '📺 Devs (2020)',
    description: 'Devs · S01E02\nEpisode 2\nDevs · S01E03\n1080p',
  };
  assert.deepEqual(streamText(session, 'Devs · S01E02'), ['Addon', '', 'Episode 2\nDevs · S01E03\n1080p']);
});

async function resolve(type, id, preset) {
  const elements = {
    'resolve-form': {}, 'resolve-status': {}, 'content-type': { value: type },
    'content-id': { value: id }, preset: { selectedOptions: [preset] },
  };
  let payload;
  const context = vm.createContext({
    $: key => elements[key],
    api: async (route, body) => { assert.equal(route, '/api/resolve'); payload = body; return { streams: [{}] }; },
  });
  vm.runInContext(resolverCode, context);
  const button = {};
  await elements['resolve-form'].onsubmit({ preventDefault() {}, target: { querySelector: () => button } });
  assert.equal(button.disabled, false);
  return JSON.parse(JSON.stringify(payload));
}

test('dashboard supplies the selected preset title only for its matching content ID and type', async () => {
  const inception = { value: 'movie:27205', textContent: ' Inception ' };
  assert.deepEqual(await resolve('movie', ' tmdb:27205 ', inception), { type: 'movie', id: 'tmdb:27205', contentName: 'Inception' });
  assert.equal((await resolve('movie', '27205', inception)).contentName, 'Inception');
  assert.equal((await resolve('movie', 'tmdb:603', inception)).contentName, '');
  assert.equal((await resolve('movie', 'tt1375666', inception)).contentName, '');
  assert.equal((await resolve('series', 'tmdb:27205:1:1', inception)).contentName, '');
  assert.equal((await resolve('movie', 'tmdb:27205', { value: '', textContent: 'Custom ID' })).contentName, '');
  assert.equal((await resolve('series', 'tmdb:81349:2:3', { value: 'series:81349', textContent: 'Devs' })).contentName, 'Devs');
});
