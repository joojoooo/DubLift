const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const app = fs.readFileSync('internal/dublift/web/app.js', 'utf8');
const html = fs.readFileSync('internal/dublift/web/index.html', 'utf8');

test('next episode settings load, disable, save, and reload', async () => {
  const nodes = new Map();
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, { value: '', checked: false, children: [] });
    return nodes.get(id);
  };
  let stored = { configVersion: 3, nextEpisodePrefetch: false, nextEpisodeLeadMinutes: 12, minConfidence: 0.68, sources: [] };
  const context = vm.createContext({ $, settings: null, sourceTypes: [],
    api: async (url, value) => {
      if (url === '/api/source-types') return [];
      if (value) stored = structuredClone(value);
      return structuredClone(stored);
    }, renderSources() {}, readSources: () => [], waitSettingsWrites: async () => {},
    queueSourceSave: async (container, build) => { stored = structuredClone(build()); }, toast() {},
  });
  vm.runInContext(app.slice(app.indexOf('const fields ='), app.indexOf('function renderManifestOptions(')), context);
  await vm.runInContext('loadSettings()', context);
  assert.equal($('next-episode-prefetch').checked, false);
  assert.equal($('next-episode-lead-minutes').value, 12);
  assert.equal($('next-episode-lead-minutes').disabled, true);
  $('next-episode-prefetch').checked = true;
  $('next-episode-prefetch').onchange();
  assert.equal($('next-episode-lead-minutes').disabled, false);
  $('next-episode-lead-minutes').value = '7';
  await $('settings-form').onsubmit({ preventDefault() {} });
  assert.equal(stored.nextEpisodePrefetch, true);
  assert.equal(stored.nextEpisodeLeadMinutes, 7);
  $('next-episode-prefetch').checked = false;
  await $('settings-form').onsubmit({ preventDefault() {} });
  await vm.runInContext('loadSettings()', context);
  assert.equal($('next-episode-prefetch').checked, false);
  assert.equal($('next-episode-lead-minutes').value, 7);
  assert.match(html, /id="next-episode-lead-minutes"[^>]*min="1"[^>]*max="30"[^>]*value="5"/);
});

function automaticDashboard() {
  const nodes = new Map();
  const $ = id => {
    if (!nodes.has(id)) nodes.set(id, { textContent: '', dataset: {}, hidden: true, open: false, setAttribute(key, value) { this[key] = value; } });
    return nodes.get(id);
  };
  const context = vm.createContext({ $, setText: (el, value) => { el.textContent = value; } });
  vm.runInContext(app.slice(app.indexOf('function renderAutomaticNextEpisode('), app.indexOf('let lastEvents =')), context);
  const render = (view, sessions = []) => {
    context.view = view;
    context.sessions = sessions;
    vm.runInContext('renderAutomaticNextEpisode(view, sessions)', context);
    return $('automatic-next-episode').hidden;
  };
  return { $, render };
}

test('automatic preparation shows the current stage without technical clutter', () => {
  const { $, render } = automaticDashboard();
  render({ enabled: true, state: 'waiting', leadMinutes: 5, trigger: 'Starts when the last requested area reaches the final 5 minutes of this episode.' });
  assert.equal($('automatic-next-episode-state').textContent, 'Scheduled');
  assert.equal($('automatic-next-episode-message').textContent, 'Starts about 5 minutes before this episode ends.');
  assert.equal($('automatic-next-episode-steps').hidden, true);
  const stages = ['resolving', 'searching', 'preparing', 'aligning'];
  const base = { enabled: true, sourceName: '<source>', content: 'tmdb:100:1:2', contentName: 'Devs · S01E02', streamCount: 3, streamName: '<matching stream>', trigger: 'One attempt per playback session.' };
  for (const [state, badge] of [['queued', 'Queued'], ['resolving', 'Finding episode'], ['searching', 'Finding stream'], ['preparing', 'Preparing video'], ['aligning', 'Checking audio']]) {
    render({ ...base, state, sampleIndex: 2, sampleTotal: 3, samplePhase: 'Downloading and decoding Vixsrc English audio near 50s · limit 10m' });
    assert.equal($('automatic-next-episode-state').textContent, badge);
    assert.equal($('automatic-next-episode')['aria-busy'], 'true');
    assert.equal($('automatic-next-episode-episode').textContent, 'Devs · S01E02');
    assert.equal($('automatic-next-episode-source').textContent, 'From <source>');
    assert.equal($('automatic-next-episode-stream').textContent, '<matching stream>');
    assert.equal($('automatic-next-episode-steps').hidden, false);
    assert.doesNotMatch($('automatic-next-episode-message').textContent, /tmdb:|binge|attempt|limit|English/);
    for (const [index, stage] of stages.entries()) {
      const step = $('automatic-step-' + stage);
      assert.equal(step.dataset.state, index < stages.indexOf(state) ? 'complete' : state === stage ? 'current' : 'upcoming');
      assert.equal(step['aria-current'], state === stage ? 'step' : 'false');
      assert.match(step['aria-label'], /complete|in progress|not started/);
    }
    assert.equal($('automatic-next-episode-progress').hidden, state !== 'aligning');
  }
  assert.equal($('automatic-next-episode-progress').textContent, 'Downloading audio samples · Check 2 of 3');
  render({ ...base, state: 'aligning', sampleIndex: 9, sampleTotal: 3, samplePhase: 'Calculating English audio alignment · limit 2m' });
  assert.equal($('automatic-next-episode-progress').textContent, 'Comparing audio timing · Check 3 of 3');
  render({ ...base, state: 'ready', audioTiming: 'Audio timing matched: +2.400 seconds · 95% confidence.', expires: '2026-11-15T12:45:00Z' });
  assert.equal($('automatic-next-episode-state').textContent, 'Ready to play');
  assert.equal($('automatic-next-episode')['aria-busy'], 'false');
  assert.equal($('automatic-next-episode-timing').textContent, 'Audio timing matched');
  assert.match($('automatic-next-episode-audio-detail').textContent, /\+2.400/);
  assert.match($('automatic-next-episode-expires').textContent, /Ready until/);
  assert.equal($('automatic-next-episode-progress').textContent, '');
  assert.equal($('automatic-next-episode-progress').hidden, true);
  for (const stage of stages) assert.equal($('automatic-step-' + stage).dataset.state, 'complete');
  assert.match(html, /id="automatic-next-episode"[^>]*aria-labelledby/);
  assert.match(html, /id="automatic-next-episode-details"[^>]*hidden>[\s\S]*?<summary>Stream details<\/summary>/);
  assert.match(app, /renderAutomaticNextEpisode\(state\.automaticNextEpisode, state\.sessions\)/);
});

test('ready audio summaries distinguish matched, manual, original, and unmatched timing', () => {
  const { $, render } = automaticDashboard();
  for (const [audioTiming, summary] of [
    ['Audio timing matched: +2.400 seconds · 95% confidence.', 'Audio timing matched'],
    ['Saved manual audio delay: -1.000 seconds.', 'Saved audio delay'],
    ['Original source audio and subtitle timing.', 'Original audio'],
    ['Offset 0 fallback; reliable automatic alignment is unavailable.', 'Audio sync needs checking'],
  ]) {
    render({ enabled: true, state: 'ready', audioTiming, expires: 'invalid' });
    assert.equal($('automatic-next-episode-timing').textContent, summary);
    assert.equal($('automatic-next-episode-expires').hidden, true);
    assert.equal($('automatic-next-episode-details').hidden, false);
    if (summary === 'Audio sync needs checking') {
      assert.equal($('automatic-next-episode').dataset.audio, 'unmatched');
      assert.equal($('automatic-step-aligning').dataset.state, 'warning');
      assert.match($('automatic-next-episode-message').textContent, /Check audio sync/);
    } else {
      assert.equal($('automatic-next-episode').dataset.audio, '');
      assert.doesNotMatch($('automatic-next-episode-message').textContent, /could not be matched/);
    }
  }
});

test('outcomes give a useful explanation and hide stale progress', () => {
  const { $, render } = automaticDashboard();
  for (const [state, badge, message, expected] of [
    ['handedOff', 'Sent to player', '', /Your player controls when playback starts/],
    ['failed', "Couldn't prepare", 'The selected source returned no usable stream list; ordinary discovery remains available.', /source did not return a usable stream/],
    ['unavailable', 'Not available', 'No stream matches the current exact binge group.', /No matching version/],
    ['unavailable', 'Not available', 'No released next episode is known, or episode metadata is unavailable.', /No released next episode/],
    ['canceled', 'Stopped', 'The requested playback area left the preparation interval.', /moved away from the end/],
    ['canceled', 'Stopped', 'Automatic preparation canceled because the player requested streams for a different episode or title.', /player requested/],
    ['expired', 'Expired', '', /Open the next episode in your player/],
    ['selected', 'Playing', '', /next episode has started/],
  ]) {
    render({ enabled: true, state, message, samplePhase: 'Stale progress', audioTiming: 'Audio timing matched: +2.400 seconds.' });
    assert.equal($('automatic-next-episode-state').textContent, badge);
    assert.match($('automatic-next-episode-message').textContent, expected);
    assert.equal($('automatic-next-episode')['aria-busy'], 'false');
    assert.equal($('automatic-next-episode-progress').hidden, true);
    assert.equal($('automatic-next-episode-steps').hidden, state !== 'handedOff');
    if (state !== 'handedOff') assert.equal($('automatic-next-episode-timing').hidden, true);
  }
  render({ enabled: false, state: 'canceled' });
  assert.equal($('automatic-next-episode-state').textContent, 'Off');
  render(undefined);
  assert.equal($('automatic-next-episode-meta').hidden, true);
  assert.equal($('automatic-next-episode-details').hidden, true);
});

test('waiting explains blockers without exposing playback internals', () => {
  const { $, render } = automaticDashboard();
  for (const [trigger, expected] of [
    ['This playing stream has no binge group for next-episode matching.', /does not support automatic/],
    ["The playing stream's source is no longer enabled or available.", /source is disabled or unavailable/],
    ['One attempt per playback session; repeated media requests do not start another search.', /No further preparation is scheduled/],
    ['Waiting for a known episode duration.', /Waiting for the episode length/],
    ['Waiting for a valid requested playback area.', /once playback progress is available/],
  ]) {
    render({ enabled: true, state: 'waiting', trigger, leadMinutes: 5 });
    assert.match($('automatic-next-episode-message').textContent, expected);
    assert.doesNotMatch($('automatic-next-episode-message').textContent, /binge group|media requests|requested area/);
  }
  render({ enabled: true, state: 'waiting', leadMinutes: 1 });
  assert.match($('automatic-next-episode-message').textContent, /1 minute before/);
});

test('handed-off episode becomes a fresh waiting target once it is playing', () => {
  const { $, render } = automaticDashboard();
  render({ enabled: true, state: 'handedOff', content: 'tmdb:100:1:2', streamName: 'Prepared stream', audioTiming: 'Audio timing matched: +2.400 seconds.' });
  $('automatic-next-episode-details').open = true;
  render({ enabled: true, state: 'handedOff', content: 'tmdb:100:1:2', streamName: 'Prepared stream' });
  assert.equal($('automatic-next-episode-details').open, true, 'polling must preserve an expanded disclosure');
  render({ enabled: true, state: 'waiting', content: 'tmdb:100:2:1', contentName: 'Next episode · S02E01', parentContent: 'tmdb:100:1:2', leadMinutes: 5 });
  assert.equal($('automatic-next-episode-state').textContent, 'Scheduled');
  assert.equal($('automatic-next-episode')['aria-busy'], 'false');
  assert.equal($('automatic-next-episode-episode').textContent, 'Next episode · S02E01');
  assert.equal($('automatic-next-episode-details').open, false);
  assert.equal($('automatic-next-episode-details').hidden, true);
  for (const id of ['stream', 'progress', 'timing', 'expires', 'audio-detail']) assert.equal($(`automatic-next-episode-${id}`).textContent, '');
  render({ enabled: true, state: 'waiting', parentContent: 'tmdb:100:2:1', message: 'The episode after the one now playing is not yet known.' });
  assert.equal($('automatic-next-episode-episode').textContent, 'Next episode');
  render({ enabled: true, state: 'resolving', content: 'tt2879552:0:3' });
  assert.equal($('automatic-next-episode-episode').textContent, 'Season 0 · Episode 3');
});

test('automatic next episode section requires enabled prefetch and active series playback', () => {
  const { render } = automaticDashboard();
  const series = { playing: true, contentType: 'series' };
  const movie = { playing: true, contentType: 'movie' };
  const prepared = { playing: false, preparationStarted: true, contentType: 'series' };
  const listed = { playing: false, contentType: 'series' };
  const view = { enabled: true, state: 'ready', content: 'tt2879552:1:3' };
  assert.equal(render(view, [series]), false);
  assert.equal(render({ ...view, enabled: false }, [series]), true);
  assert.equal(render(view, [series]), false, 're-enabling should restore the section');
  assert.equal(render(view, []), true);
  assert.equal(render(view, [movie]), true);
  assert.equal(render(view, [listed]), true);
  assert.equal(render(view, [prepared]), true, 'preparation alone is not playback');
  assert.equal(render(view, [movie, listed, prepared]), true);
  assert.equal(render(view, [movie, series]), false);
  assert.equal(render({ enabled: true, state: 'canceled' }, [movie]), true, 'retained outcomes must stay hidden outside series playback');
  assert.equal(render(undefined, [series]), true, 'unknown settings must not reveal the section');
  assert.equal(render(view, undefined), true, 'unknown playback must not reveal the section');
  assert.match(html, /id="automatic-next-episode"[^>]*\bhidden\b/);
});
