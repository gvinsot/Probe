// Probe Hub dashboard.
//
// The page holds three things: the repository list with its bootstrap actions,
// the commit graph and cached plan/normal results, and a live event stream
// that refreshes results without changing the selected commit.
'use strict';

const LEVELS = ['low', 'medium', 'high', 'critical'];
const KINDS = [
  { key: 'all', label: 'Everything' },
  { key: 'issue', label: 'Issues' },
  { key: 'check', label: 'Checks' },
  { key: 'signal', label: 'Signals' },
  { key: 'focus', label: 'Review plan' },
];

// The review threshold is one account-wide preference: it applies to every
// repository and survives reloads.
const SEVERITY_KEY = 'probe.hub.minSeverity';

// The period is the other account-wide preference: each repository shows the
// most severe status among the commits analyzed during it. The server sends
// the runs of the longest period with the repository list.
const PERIODS = [
  { label: '1h', hours: 1 },
  { label: '6h', hours: 6 },
  { label: '12h', hours: 12 },
  { label: '1d', hours: 24 },
  { label: '2d', hours: 48 },
  { label: '3d', hours: 72 },
  { label: '7d', hours: 168 },
  { label: '10d', hours: 240 },
];
const REPO_FILTERS_KEY = 'probe.hub.repoFilters';
const PERIOD_KEY = 'probe.hub.period';

const state = {
  me: null,
  csrf: '',
  repos: new Map(),
  repoKey: null,
  commit: null,
  view: null,
  run: null,
  variant: "normal",
  graphs: new Map(),
  runs: [],
  // Queued or running attempts by pendingKey, each tagged with its repo_key.
  // The server's history supersedes an entry once the attempt is finished.
  pending: new Map(),
  // Enqueue time (ms) of the latest finished attempt by pendingKey, so a late
  // or replayed event cannot bring back a "queued" badge.
  settled: new Map(),
  loadID: 0,
  reportID: 0,
  minSeverity: loadMinSeverity(),
  period: loadPeriod(),
  // Recent normal runs per repository key, keyed by commit, kept up to date
  // by the live events.
  recent: new Map(),
  recentLimit: null,
  plan: null,
  kind: 'all',
  query: '',
  onlyMonitored: false,
  onlyPolicy: false,
  onlyMissing: false,
  expanded: new Set(),
  // Whether the list of what the AI reviewer set aside is unfolded.
  showDismissed: false,
};

const el = (id) => document.getElementById(id);

/* ------------------------------------------------------------------ API -- */

async function api(path, options = {}) {
  const init = {
    credentials: 'same-origin',
    headers: Object.assign({ 'Accept': 'application/json' }, options.headers || {}),
    method: options.method || 'GET',
  };
  if (options.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(options.body);
  }
  if (init.method !== 'GET' && init.method !== 'HEAD') {
    init.headers['X-Probe-CSRF'] = state.csrf;
  }
  const response = await fetch(path, init);
  if (response.status === 401) {
    window.location.replace('/index.html');
    throw new Error('signed out');
  }
  const text = await response.text();
  const payload = text ? JSON.parse(text) : {};
  if (!response.ok) {
    throw new Error(payload.error || ('request failed with ' + response.status));
  }
  return payload;
}

function toast(message, isError) {
  const holder = el('toast');
  const item = document.createElement('div');
  if (isError) item.classList.add('error');
  item.textContent = message;
  holder.appendChild(item);
  setTimeout(() => item.remove(), isError ? 9000 : 4500);
}

/* ------------------------------------------------------------- rendering -- */

function severityClass(severity) { return 'sev-' + (severity || 'low'); }

function chip(text, cls) {
  const span = document.createElement('span');
  span.className = 'chip' + (cls ? ' ' + cls : '');
  span.textContent = text;
  return span;
}

function dotChip(text, severity) {
  const span = chip('', '');
  const dot = document.createElement('span');
  dot.className = 'dot';
  span.classList.add(severityClass(severity));
  span.appendChild(dot);
  span.appendChild(document.createTextNode(text));
  return span;
}

function shortSha(sha) { return sha ? sha.slice(0, 10) : ''; }

function timeAgo(value) {
  if (!value) return '';
  const then = new Date(value).getTime();
  if (Number.isNaN(then)) return '';
  const seconds = Math.max(1, Math.round((Date.now() - then) / 1000));
  if (seconds < 60) return seconds + 's ago';
  if (seconds < 3600) return Math.round(seconds / 60) + 'm ago';
  if (seconds < 86400) return Math.round(seconds / 3600) + 'h ago';
  return Math.round(seconds / 86400) + 'd ago';
}

// A review verdict takes the level of the most severe alert in the report; a
// review requested without any alert (incomplete checks, unverified areas)
// counts as medium.
function reviewLevel(summary) {
  const counts = summary.counts || {};
  const top = LEVELS.slice().reverse().find((level) => counts[level] > 0);
  return top || 'medium';
}

function reviewTone(summary) { return 'tone-' + reviewLevel(summary); }

// belowThreshold tells whether a review verdict stays under the selected
// severity, in which case the hub does not flag it for a human. The verdict
// itself is the CLI's; only its presentation follows the preference.
function belowThreshold(summary) {
  return LEVELS.indexOf(reviewLevel(summary)) < state.minSeverity;
}

// needsReview is true for a finished run the current threshold flags.
function needsReview(run) {
  const summary = run && run.status === 'done' && run.summary;
  return Boolean(summary && summary.verdict === 'review' && !belowThreshold(summary));
}

function belowThresholdChip(summary) {
  const quiet = chip('review below ' + LEVELS[state.minSeverity], 'below-threshold');
  quiet.title = 'Human review was requested at ' + reviewLevel(summary) + ' level, under the selected threshold.';
  return quiet;
}

function verdictChip(run) {
  if (!run) { const unknown = chip('?', 'unknown'); unknown.title = 'No cached result'; return unknown; }
  if (run.status === 'queued') return chip('queued', 'busy');
  if (run.status === 'running') return chip('analyzing…', 'busy');
  if (run.status === 'failed') return chip('analysis failed', 'bad');
  if (run.status === 'cancelled') return chip('cancelled');
  const summary = run.summary || {};
  switch (summary.verdict) {
    case 'blocked': return chip('reproduced issue', 'bad');
    case 'review':
      if (belowThreshold(summary)) return belowThresholdChip(summary);
      return chip('Human review required', 'warn ' + reviewTone(summary));
    case 'clear': return chip(run.variant === 'plan' ? 'No plan category flagged' : 'no blocker', 'ok');
    default: return chip(summary.verdict || 'unknown');
  }
}

/* ------------------------------------------------------- repository list -- */

// statusRank orders run statuses by gravity so the worst of a period wins:
// a reproduced issue, then flagged reviews by level, a failed analysis,
// reviews under the threshold, pending analyses and finally clear results.
function statusRank(run) {
  if (run.status === 'queued' || run.status === 'running') return 1;
  if (run.status === 'failed') return 20;
  const summary = run.summary || {};
  switch (summary.verdict) {
    case 'blocked': return 40;
    case 'review': {
      const level = LEVELS.indexOf(reviewLevel(summary));
      return belowThreshold(summary) ? 10 + level : 30 + level;
    }
    case 'clear': return 0;
    default: return 2;
  }
}

// Zero Go timestamps, absent values and invalid dates are not activity dates.
function runTimestamp(value) {
  if (!value) return 0;
  const at = new Date(value).getTime();
  return Number.isFinite(at) && at > 0 ? at : 0;
}

function runActivity(run) {
  return Math.max(runTimestamp(run.queued_at), runTimestamp(run.finished_at));
}

function runInPeriod(run, since) {
  const at = runActivity(run);
  return run.status === 'queued' || run.status === 'running' || !at || at >= since;
}

// Store only the projection needed by the repository list, even for full SSE
// reports. A partial/zero timestamp must not erase a known date.
function mergeRecentRun(previous = {}, run) {
  const merged = { ...previous };
  for (const key of ['commit', 'status', 'variant']) {
    if (run[key] !== undefined && run[key] !== null) merged[key] = run[key];
  }
  for (const key of ['queued_at', 'finished_at']) {
    if (runTimestamp(run[key])) merged[key] = run[key];
  }
  if (run.summary) {
    merged.summary = { ...previous.summary };
    if (run.summary.verdict !== undefined) merged.summary.verdict = run.summary.verdict;
    if (run.summary.counts) merged.summary.counts = { ...previous.summary?.counts, ...run.summary.counts };
  }
  return merged;
}

function rememberRun(repoKey, run) {
  if (!run || !run.commit || (run.variant && run.variant !== 'normal')) return;
  if (!state.recent.has(repoKey)) state.recent.set(repoKey, new Map());
  const runs = state.recent.get(repoKey);
  runs.set(run.commit, mergeRecentRun(runs.get(run.commit), run));
  const cutoff = Date.now() - PERIODS[PERIODS.length - 1].hours * 3600 * 1000;
  for (const [commit, item] of runs) {
    if (!runInPeriod(item, cutoff)) runs.delete(commit);
  }
  // Mirror the API cap for long-lived event streams, keeping undated/pending
  // results first and explicitly marking any omitted history.
  if (state.recentLimit && runs.size > state.recentLimit) {
    const always = (item) => item.status === 'queued' || item.status === 'running' || !runActivity(item);
    const newest = Array.from(runs.values()).sort((a, b) => Number(always(b)) - Number(always(a)) || runActivity(b) - runActivity(a) || a.commit.localeCompare(b.commit));
    state.recent.set(repoKey, new Map(newest.slice(0, state.recentLimit).map((item) => [item.commit, item])));
    const repo = state.repos.get(repoKey);
    if (repo) repo.recent_incomplete = true;
  }
}

// Undated results and pending work stay visible at every period. Completed
// runs use their latest activity, including a finish after an old queue time.
function periodRuns(repo) {
  const since = Date.now() - PERIODS[state.period].hours * 3600 * 1000;
  return Array.from(state.recent.get(repo.key)?.values() || []).filter((run) => runInPeriod(run, since));
}

// worstRun returns the most severe run of the period; among equals, the newest.
function worstRun(repo) {
  let worst = null;
  for (const run of periodRuns(repo)) {
    if (!worst || statusRank(run) > statusRank(worst) ||
        (statusRank(run) === statusRank(worst) && runActivity(run) > runActivity(worst))) {
      worst = run;
    }
  }
  return worst;
}

// Remember the repository checkboxes in the browser between visits.
function saveRepoFilters() {
  const value = { monitored: state.onlyMonitored, policy: state.onlyPolicy, missing: state.onlyMissing };
  try { localStorage.setItem(REPO_FILTERS_KEY, JSON.stringify(value)); } catch (err) { /* storage disabled */ }
}

function restoreRepoFilters() {
  let saved = {};
  try { saved = JSON.parse(localStorage.getItem(REPO_FILTERS_KEY) || '{}') || {}; } catch (err) { /* storage disabled or corrupt */ }
  state.onlyMonitored = saved.monitored === true;
  state.onlyPolicy = saved.policy === true;
  state.onlyMissing = saved.missing === true && !state.onlyPolicy;
  el('only-monitored').checked = state.onlyMonitored;
  el('only-policy').checked = state.onlyPolicy;
  el('only-nopolicy').checked = state.onlyMissing;
}

function visibleRepos() {
  const query = state.query.trim().toLowerCase();
  return Array.from(state.repos.values()).filter((repo) => {
    if (query && !repo.full_name.toLowerCase().includes(query)) return false;
    if (state.onlyMonitored && !repo.monitored) return false;
    if (state.onlyPolicy && !repo.has_policy) return false;
    if (state.onlyMissing && repo.has_policy) return false;
    return true;
  }).sort((a, b) => {
    if (a.monitored !== b.monitored) return a.monitored ? -1 : 1;
    return a.full_name.localeCompare(b.full_name);
  });
}

function renderRepos() {
  const list = el('repos');
  const repos = visibleRepos();
  list.textContent = '';
  el('repo-count').textContent = String(state.repos.size);
  el('repos-empty').classList.toggle('hidden', repos.length > 0);
  renderOutdatedNotice();
  renderRepoInfo();
  renderReviewCount();

  for (const repo of repos) {
    const item = document.createElement('li');
    item.className = 'repo' + (repo.key === state.repoKey ? ' active' : '');
    item.tabIndex = 0;

    // The title row carries the repository actions, so each entry stays on
    // two lines: name and actions, then branch and latest verdict.
    const head = document.createElement('div');
    head.className = 'repo-head';
    const name = document.createElement('div');
    name.className = 'repo-name';
    name.appendChild(document.createTextNode(repo.full_name));
    if (repo.private) name.appendChild(chip('private'));
    head.appendChild(name);
    head.appendChild(repoActions(repo));
    item.appendChild(head);

    const meta = document.createElement('div');
    meta.className = 'repo-meta';
    meta.appendChild(chip(repo.default_branch || 'no branch'));
    if (repo.hook_outdated) meta.appendChild(chip('webhook to reinstall', 'warn'));
    // The status is the most severe one among the commits of the period.
    const runs = periodRuns(repo);
    const worst = worstRun(repo);
    const period = PERIODS[state.period].label;
    if (worst) {
      meta.appendChild(verdictChip(worst));
      if (worst.summary && worst.summary.counts && worst.status === 'done') {
        const counts = worst.summary.counts;
        if (counts.total > 0) meta.appendChild(dotChip(counts.total + ' alerts', worstSeverity(counts)));
      }
      const scope = chip(runs.length + (runs.length === 1 ? ' commit' : ' commits') + ' · ' + period);
      scope.title = 'Most severe status among the commits analyzed in the last ' + period +
        (worst.commit ? ' (' + shortSha(worst.commit) + ')' : '');
      meta.appendChild(scope);
      if (runs.some((run) => !runActivity(run))) {
        const undated = chip('analysis date unknown', 'warn');
        undated.title = 'Undated results remain visible in every period.';
        meta.appendChild(undated);
      }
    } else {
      const none = chip('no commit in ' + period, 'unknown');
      none.title = 'No analysis in the selected period';
      meta.appendChild(none);
    }
    if (repo.recent_incomplete) {
      const partial = chip('partial history', 'warn');
      partial.title = 'Only retained recent analyses are shown; older results may be missing. The displayed status is not exhaustive.';
      meta.appendChild(partial);
    }
    if (repo.latest && repo.latest.finished_at) {
      const last = chip(timeAgo(repo.latest.finished_at));
      last.title = 'Latest analysis';
      meta.appendChild(last);
    }
    item.appendChild(meta);

    const open = () => selectRepo(repo.key);
    item.addEventListener('click', open);
    item.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); open(); }
    });
    list.appendChild(item);
  }
}

// renderReviewCount shows, in the top bar, how many repositories have a result
// the threshold flags in the selected period, and how many commits of the
// selected repository (among its cached results) do.
function renderReviewCount() {
  const repos = Array.from(state.repos.values()).filter((repo) => needsReview(worstRun(repo))).length;
  const parts = [repos + (repos === 1 ? ' repository' : ' repositories')];
  if (state.repoKey && state.graphs.has(state.repoKey)) {
    const commits = new Set(state.runs.filter(needsReview).map((run) => run.commit)).size;
    parts.push(commits + (commits === 1 ? ' commit' : ' commits'));
  }
  const count = el('review-count');
  count.textContent = parts.join(' · ') + ' to review';
  if (Array.from(state.repos.values()).some((repo) => repo.recent_incomplete)) count.textContent += ' · partial history';
  count.classList.toggle('warn', repos > 0);
}

// repoActions builds the buttons shown to the right of a repository name.
// Adding a policy and activating monitoring are separate actions.
function repoActions(repo) {
  const actions = document.createElement('div');
  actions.className = 'repo-actions';
  const noAdmin = 'Your account cannot manage webhooks on this repository';
  if (!repo.has_policy) {
    const addPolicy = button('Add policy', 'btn setup small', (event) => {
      event.stopPropagation();
      openPolicyDialog(repo);
    });
    addPolicy.title = 'Create a .probe.json policy';
    actions.appendChild(addPolicy);
    return actions;
  }
  if (!repo.monitored) {
    const monitor = button('Activate monitoring', 'btn monitor small', (event) => {
      event.stopPropagation();
      setMonitoring(repo, true, monitor);
    });
    monitor.disabled = !repo.admin;
    if (!repo.admin) monitor.title = noAdmin;
    actions.appendChild(monitor);
  } else if (repo.hook_outdated) {
    const reinstall = button('Reinstall webhook', 'btn small', (event) => {
      event.stopPropagation();
      setMonitoring(repo, true, reinstall);
    });
    reinstall.disabled = !repo.admin;
    if (!repo.admin) reinstall.title = noAdmin;
    actions.appendChild(reinstall);
  }
  actions.appendChild(button('Analyze now', 'btn ghost small', (event) => {
    event.stopPropagation();
    analyzeNow(repo);
  }));
  if (repo.monitored) {
    actions.appendChild(button('Stop monitoring', 'btn quiet small', (event) => {
      event.stopPropagation();
      setMonitoring(repo, false);
    }));
  }
  return actions;
}

// renderOutdatedNotice tells the owner which webhooks predate installation
// tokens: the forge still delivers them but the hub refuses every one.
function renderOutdatedNotice() {
  const notice = el('hooks-outdated');
  const outdated = Array.from(state.repos.values()).filter((repo) => repo.hook_outdated);
  notice.classList.toggle('hidden', outdated.length === 0);
  if (outdated.length === 0) return;
  const names = outdated.map((repo) => repo.full_name).sort();
  notice.textContent = (outdated.length === 1 ? 'The webhook of ' : 'The webhooks of ') + names.join(', ') +
    (outdated.length === 1 ? ' was' : ' were') + ' installed by an earlier version of the hub and every push it delivers is now refused. ' +
    'Use “Reinstall webhook” to resume analyses.';
}

// renderRepoInfo shows any pending webhook action for the selected repository.
function renderRepoInfo() {
  const box = el('repo-info');
  const repo = state.repos.get(state.repoKey);
  box.textContent = '';
  if (!repo || !repo.monitored || !repo.hook_outdated) {
    box.classList.add('hidden');
    return;
  }
  box.classList.remove('hidden');
  const notice = document.createElement('p');
  notice.className = 'notice';
  notice.textContent = 'This webhook predates installation tokens: the hub refuses its deliveries. ' +
    'Reinstall it from the repository list to resume analyses.';
  box.appendChild(notice);
}

function worstSeverity(counts) {
  if (counts.critical > 0) return 'critical';
  if (counts.high > 0) return 'high';
  if (counts.medium > 0) return 'medium';
  return 'low';
}

function button(label, className, onClick) {
  const b = document.createElement('button');
  b.className = className;
  b.textContent = label;
  b.addEventListener('click', onClick);
  return b;
}

/* ----------------------------------------------------- repository actions -- */

async function openPolicyDialog(repo) {
  closeModal();
  const body = el('modal-body');
  const footer = el('modal-footer');
  el('modal-title').textContent = 'Add policy to ' + repo.full_name;
  body.textContent = '';
  footer.textContent = '';

  const intro = document.createElement('p');
  intro.className = 'note';
  intro.textContent = 'A .probe.json policy will be generated by the Probe CLI this service runs, and committed on '
    + (repo.default_branch || 'the default branch')
    + '. Monitoring can be activated separately after adding the policy. Review the sandbox image and the commands before relying on a report.';
  body.appendChild(intro);

  const row = document.createElement('div');
  row.className = 'row';
  const label = document.createElement('label');
  label.className = 'note';
  label.textContent = 'Language';
  const select = document.createElement('select');
  select.className = 'select';
  for (const language of ['auto', 'go', 'typescript', 'javascript', 'python', 'unknown']) {
    const option = document.createElement('option');
    option.value = language;
    option.textContent = language;
    select.appendChild(option);
  }
  row.appendChild(label);
  row.appendChild(select);
  body.appendChild(row);

  const preview = document.createElement('pre');
  preview.className = 'policy';
  preview.textContent = 'Generating a preview…';
  body.appendChild(preview);

  const create = button('Commit the policy', 'btn', async () => {
    create.disabled = true;
    try {
      const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/policy', {
        method: 'POST',
        body: { language: select.value === 'auto' ? '' : select.value },
      });
      upsertRepo(payload.repo);
      closeModal();
      toast('Committed .probe.json on ' + repo.default_branch + '.');
    } catch (err) {
      toast(err.message, true);
      create.disabled = false;
    }
  });
  footer.appendChild(button('Cancel', 'btn quiet', closeModal));
  footer.appendChild(create);

  const loadPreview = async () => {
    preview.textContent = 'Generating a preview…';
    create.disabled = true;
    try {
      const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/policy', {
        method: 'POST',
        body: { language: select.value === 'auto' ? '' : select.value, preview: true },
      });
      preview.textContent = payload.policy;
      if (select.value === 'auto') {
        intro.textContent = 'Detected language: ' + payload.language + '. ' + intro.textContent;
      }
      create.disabled = false;
    } catch (err) {
      preview.textContent = err.message;
    }
  };
  select.addEventListener('change', loadPreview);
  el('modal').classList.remove('hidden');
  loadPreview();
}

let activityTimer;
let activityDialogID = 0;
let activityRequestID = 0;
let activityOpen = false;

function closeModal() {
  el('modal').classList.add('hidden');
  clearInterval(activityTimer);
  activityDialogID++;
  if (activityOpen) el('analyses').focus();
  activityOpen = false;
}

function openActivityDialog() {
  closeModal();
  activityOpen = true;
  el('modal-title').textContent = 'Analyses · last 48 hours';
  el('modal-body').textContent = '';
  const note = document.createElement('p');
  note.className = 'note';
  note.textContent = 'Queued and running analyses, plus results from the last 48 hours. History is cleared when the hub restarts.';
  const status = document.createElement('p');
  status.id = 'activity-status';
  status.className = 'note';
  status.setAttribute('role', 'status');
  status.textContent = 'Loading analyses…';
  const list = document.createElement('div');
  list.id = 'activity-list';
  el('modal-body').append(note, status, list);
  el('modal-footer').replaceChildren(button('Refresh', 'btn small', refreshActivity));
  el('modal').classList.remove('hidden');
  el('modal-close').focus();
  refreshActivity();
  activityTimer = setInterval(refreshActivity, 5000);
}

async function refreshActivity() {
  if (!activityOpen) return;
  const dialogID = activityDialogID, requestID = ++activityRequestID;
  try {
    const data = await api('/api/analyses');
    if (!activityOpen || dialogID !== activityDialogID || requestID !== activityRequestID) return;
    renderActivity(data.analyses || []);
    el('activity-status').textContent = 'Updated ' + new Date().toLocaleTimeString() + ' · refreshes every 5 seconds';
  } catch (err) {
    if (activityOpen && dialogID === activityDialogID && requestID === activityRequestID) {
      el('activity-status').textContent = 'Could not refresh analyses: ' + err.message;
    }
  }
}

function renderActivity(items) {
  const list = el('activity-list');
  list.textContent = '';
  for (const [label, statuses] of [['Queued', ['queued']], ['Running', ['running']], ['Past analyses', ['done', 'failed', 'cancelled']]]) {
    const group = items.filter((item) => statuses.includes(item.status));
    const section = document.createElement('section');
    const heading = document.createElement('h4');
    heading.textContent = label + ' (' + group.length + ')';
    section.appendChild(heading);
    if (!group.length) {
      const empty = document.createElement('p');
      empty.className = 'note';
      empty.textContent = 'No analyses.';
      section.appendChild(empty);
    }
    for (const item of group) {
      const row = document.createElement('article');
      row.className = 'activity-row';
      const title = document.createElement('div');
      title.className = 'row';
      const repo = state.repos.get(item.repo_key);
      const name = document.createElement('strong');
      name.textContent = (repo ? repo.full_name : item.repo_key) + ' · ' + shortSha(item.commit);
      name.title = item.commit;
      const statusLabel = { queued: 'Queued', running: 'Running', done: 'Completed', failed: 'Failed', cancelled: 'Cancelled' }[item.status];
      title.append(name, chip(item.variant === 'plan' ? 'Plan' : 'Analysis'), chip(statusLabel, item.status === 'failed' ? 'bad' : ''));
      const dates = document.createElement('p');
      dates.className = 'note';
      dates.textContent = [['Queued', item.queued_at], ['Started', item.started_at], ['Finished', item.finished_at]]
        .filter(([, value]) => value && !value.startsWith('0001-'))
        .map(([label, value]) => label + ' ' + new Date(value).toLocaleString()).join(' · ');
      row.append(title, dates);
      if (item.mode || item.trigger) {
        const detail = document.createElement('p');
        detail.className = 'note';
        detail.textContent = [item.mode ? analysisModeLabel(item.mode) : '', item.trigger].filter(Boolean).join(' · ');
        row.appendChild(detail);
      }
      if (item.error) {
        const error = document.createElement('p');
        error.className = 'activity-error';
        error.textContent = item.error;
        row.appendChild(error);
      }
      const actions = document.createElement('div');
      actions.className = 'row';
      if (item.status === 'queued') actions.appendChild(button('Cancel', 'btn quiet small', (e) => cancelAnalysis(item, e.currentTarget)));
      // A cancelled attempt left no stored parameters to run again with.
      if (item.status === 'done' || item.status === 'failed') actions.appendChild(button('Run again', 'btn quiet small', (e) => rerunAnalysis(item, e.currentTarget)));
      if (repo) actions.appendChild(button('Open commit', 'btn quiet small', () => {
        closeModal();
        selectRepo(item.repo_key, item.commit);
      }));
      if (actions.childElementCount) row.appendChild(actions);
      section.appendChild(row);
    }
    list.appendChild(section);
  }
}

// cancelAnalysis withdraws an attempt still waiting in the hub queue.
async function cancelAnalysis(item, trigger) {
  trigger.disabled = true;
  try {
    await api('/api/repos/' + encodeURIComponent(item.repo_key) + '/cancel', {
      method: 'POST', body: { commit: item.commit, variant: item.variant || 'normal' },
    });
    toast('Cancelled the analysis of ' + shortSha(item.commit) + '.');
  } catch (err) {
    toast(err.message, true);
  }
  refreshActivity();
}

// rerunAnalysis queues a finished attempt again with its stored parameters.
async function rerunAnalysis(item, trigger) {
  trigger.disabled = true;
  try {
    const queued = await api('/api/repos/' + encodeURIComponent(item.repo_key) + '/rerun', {
      method: 'POST', body: { commit: item.commit, variant: item.variant || 'normal' },
    });
    followQueued(item.repo_key, queued);
    toast('Queued a new analysis of ' + shortSha(item.commit) + '.');
  } catch (err) {
    toast(err.message, true);
    trigger.disabled = false;
  }
  refreshActivity();
}

async function setMonitoring(repo, on, trigger) {
  if (trigger) trigger.disabled = true;
  try {
    const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/monitor', {
      method: on ? 'POST' : 'DELETE',
    });
    upsertRepo(payload.repo);
    followQueued(repo.key, payload.queued);
    toast(on
      ? 'Monitoring ' + repo.full_name + '. A first analysis is queued; every new commit will follow.'
      : 'Stopped monitoring ' + repo.full_name + '.');
  } catch (err) {
    toast(err.message, true);
    if (trigger) trigger.disabled = false;
  }
}

async function analyzeNow(repo) {
  try {
    const queued = await api('/api/repos/' + encodeURIComponent(repo.key) + '/analyze', { method: 'POST', body: {} });
    followQueued(repo.key, queued);
    toast('Analysis queued for ' + repo.full_name + '.');
  } catch (err) {
    toast(err.message, true);
  }
}

function upsertRepo(repo) {
  if (!repo) return;
  state.repos.set(repo.key, repo);
  renderRepos();
}

/* ------------------------------------------------------------- the report -- */

async function selectRepo(repoKey, commit) {
  const changed = state.repoKey !== repoKey || state.commit !== (commit || null);
  if (changed) closeReportDialog();
  state.repoKey = repoKey;
  state.commit = commit || null;
  state.expanded.clear();
  if (changed) state.variant = 'normal';
  const hash = '#/repo/' + repoKey + (commit ? '/commit/' + commit : '');
  if (window.location.hash !== hash) window.location.hash = hash;
  renderRepos();
  clearReport(state.repos.get(repoKey));
  el('commit-tree').textContent = '';
  el('branches').textContent = '';
  el('commit-actions').classList.add('hidden');
  await loadHistory(changed);
}

async function loadHistory(resetIntent = false) {
  const repo = state.repos.get(state.repoKey);
  if (!repo) return;
  const loadID = ++state.loadID;
  el('commit-browser').classList.remove('hidden');
  el('splitter').classList.remove('hidden');
  el('graph-note').textContent = 'Loading commits…';
  // History remains useful if fetching Git temporarily fails.
  const results = await Promise.allSettled([
    state.graphs.has(repo.key) ? Promise.resolve(state.graphs.get(repo.key))
      : api('/api/repos/' + encodeURIComponent(repo.key) + '/commits'),
    api('/api/repos/' + encodeURIComponent(repo.key) + '/runs'),
  ]);
  if (loadID !== state.loadID || repo.key !== state.repoKey) return;
  const [graphResult, runsResult] = results;
  if (runsResult.status === 'fulfilled') { state.runs = runsResult.value.runs || []; settleFromHistory(repo.key); }
  else { state.runs = []; toast(runsResult.reason.message, true); }
  if (graphResult.status === 'fulfilled') {
    state.graphs.set(repo.key, graphResult.value);
    renderGraph();
  } else {
    el('graph-note').textContent = graphResult.reason.message;
    el('commit-tree').textContent = '';
  }
  if (state.commit) {
    renderCommitActions(resetIntent);
    await loadReport();
  } else {
    clearReport(repo);
    el('report-empty').textContent = 'Select a commit to inspect cached results or launch an analysis.';
  }
}

function cachedRun(commit, variant) {
  return state.runs.find((run) => run.commit === commit && (run.variant || 'normal') === variant);
}

function pendingKey(repoKey, commit, variant) { return repoKey + '/' + commit + '/' + (variant || 'normal'); }

function displayedRun(commit, variant) {
  return state.pending.get(pendingKey(state.repoKey, commit, variant)) || cachedRun(commit, variant);
}

function isPending(run) { return Boolean(run) && (run.status === 'queued' || run.status === 'running'); }

// trackRun records the state of one attempt, whoever reported it: a live
// event, an enqueue response or the activity list. Attempts are told apart by
// their server enqueue time, so an event about an older attempt, or a late
// "queued" for an attempt already finished, changes nothing. It returns false
// for such stale updates.
function trackRun(repoKey, run) {
  if (!repoKey || !run || !run.commit) return false;
  const key = pendingKey(repoKey, run.commit, run.variant);
  const at = runTimestamp(run.queued_at);
  const floor = state.settled.get(key) || 0;
  if (at && (isPending(run) ? at <= floor : at < floor)) return false;
  const current = state.pending.get(key);
  const currentAt = runTimestamp(current?.queued_at);
  if (at && currentAt && (at < currentAt || (at === currentAt && current.status === 'running' && run.status === 'queued'))) return false;
  if (isPending(run)) {
    state.pending.set(key, { ...run, variant: run.variant || 'normal', repo_key: repoKey, requested: current?.requested || run.requested || Date.now() });
    watchPending();
  } else {
    state.pending.delete(key);
    if (at) state.settled.set(key, Math.max(at, floor));
  }
  return true;
}

// followQueued tracks the attempt an enqueue request resolved to.
function followQueued(repoKey, queued) {
  if (!queued || !queued.commit) return false;
  const run = { commit: queued.commit, variant: queued.variant || 'normal', status: 'queued', queued_at: queued.queued_at, requested: Date.now() };
  if (!trackRun(repoKey, run)) return false;
  rememberRun(repoKey, run);
  renderRepos();
  if (repoKey === state.repoKey) { renderGraph(); renderCommitActions(); }
  return true;
}

// settleFromHistory lets the stored history of a repository close the attempts
// it already holds a result for.
function settleFromHistory(repoKey) {
  for (const run of Array.from(state.pending.values())) {
    if (run.repo_key !== repoKey || !runTimestamp(run.queued_at)) continue;
    const cached = cachedRun(run.commit, run.variant);
    if (cached && !isPending(cached) && runTimestamp(cached.queued_at) >= runTimestamp(run.queued_at)) trackRun(repoKey, cached);
  }
}

// Live events normally close an attempt, but a proxy may delay or drop them.
// While anything is shown as queued or running, the activity list is polled
// so that a finished analysis never stays "queued".
const PENDING_POLL_MS = 4000;
// An attempt the hub never acknowledged is forgotten after this delay.
const PENDING_GRACE_MS = 60000;
let pendingTimer = null;

function watchPending() {
  if (pendingTimer || state.pending.size === 0) return;
  pendingTimer = setTimeout(async () => {
    try { await syncPending(); } finally { pendingTimer = null; watchPending(); }
  }, PENDING_POLL_MS);
}

async function syncPending() {
  if (state.pending.size === 0) return;
  let items;
  try { items = (await api('/api/analyses')).analyses || []; } catch (err) { return; }
  const finished = new Set();
  let changed = false;
  for (const run of Array.from(state.pending.values())) {
    const since = runTimestamp(run.queued_at);
    let latest = null;
    for (const item of items) {
      if (item.repo_key !== run.repo_key || item.commit !== run.commit || (item.variant || 'normal') !== run.variant) continue;
      // Without the server's enqueue time only work still in progress can be
      // matched: an earlier finished attempt must not close this one.
      if (since ? runTimestamp(item.queued_at) < since : !isPending(item)) continue;
      if (!latest || runTimestamp(item.queued_at) > runTimestamp(latest.queued_at)) latest = item;
    }
    if (latest) {
      if (trackRun(run.repo_key, latest)) {
        changed = true;
        if (!isPending(latest)) finished.add(run.repo_key);
      }
    } else if (since || Date.now() - run.requested > PENDING_GRACE_MS) {
      // The hub no longer knows this attempt (its activity list lives in
      // memory and a restart clears it): the stored history decides.
      state.pending.delete(pendingKey(run.repo_key, run.commit, run.variant));
      changed = true;
      finished.add(run.repo_key);
    }
  }
  if (!changed) return;
  renderRepos();
  if (state.repoKey) { renderGraph(); renderCommitActions(); }
  // The activity list carries no verdict: reload the results it announced.
  if (finished.size) refreshDashboard(finished.has(state.repoKey));
}

// Combine review requests in the tree, keeping the most severe report's
// presentation. Other statuses remain visible for each variant.
function commitVerdictChips(normal, plan) {
  if ([normal, plan].every((run) => run && run.status === 'done' && run.summary?.verdict === 'review')) {
    const result = verdictChip(statusRank(plan) > statusRank(normal) ? plan : normal);
    result.title = ['Analysis: ' + reviewLevel(normal.summary), 'Plan: ' + reviewLevel(plan.summary), result.title].filter(Boolean).join('\n');
    return [result];
  }
  return [verdictChip(normal), verdictChip(plan)];
}

function svgElement(tag, attrs) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, String(value));
  return node;
}

// Height in pixels of a commit row; the graph nodes are aligned on it.
const COMMIT_ROW = 56;

// Assign lanes from child to parent in Git's topological order; parents are
// drawn as edges.
function graphLayout(commits) {
  const lanes = [];
  const positions = new Map();
  let width = 1;
  commits.forEach((commit, row) => {
    let lane = lanes.indexOf(commit.sha);
    if (lane < 0) { lane = lanes.indexOf(null); if (lane < 0) lane = lanes.length; }
    lanes[lane] = null;
    positions.set(commit.sha, { x: 12 + lane * 18, y: row * COMMIT_ROW + 17 });
    for (const parent of commit.parents || []) {
      if (lanes.includes(parent)) continue;
      let slot = lanes.indexOf(null);
      if (slot < 0) slot = lanes.length;
      lanes[slot] = parent;
    }
    width = Math.max(width, lanes.length, lane + 1);
  });
  return { positions, width: width * 18 + 6 };
}

function renderGraph() {
  renderReviewCount();
  const graph = state.graphs.get(state.repoKey);
  if (!graph) return;
  const commits = graph.commits || [];
  el('graph-note').textContent = !commits.length ? 'This repository has no commits yet.'
    : graph.limited ? 'Recent history: up to 300 commits, fetched to a depth of 100 per branch. Older parents may be outside this view.'
      : 'All branches · select a commit · ? means no cached result.';
  const branches = el('branches');
  branches.textContent = '';
  for (const branch of graph.branches || []) {
    branches.appendChild(button(branch.name, 'btn quiet small', () => selectRepo(state.repoKey, branch.sha)));
  }
  const tree = el('commit-tree');
  tree.textContent = '';
  const { positions, width } = graphLayout(commits);
  const svg = svgElement('svg', { width, height: commits.length * COMMIT_ROW, 'aria-hidden': 'true', class: 'graph-lines' });
  const bend = COMMIT_ROW * 0.4;
  for (const commit of commits) {
    const p = positions.get(commit.sha);
    for (const parent of commit.parents || []) {
      const target = positions.get(parent);
      const q = target || { x: p.x, y: p.y + COMMIT_ROW * 0.6 };
      svg.appendChild(svgElement('path', { d: `M ${p.x} ${p.y} C ${p.x} ${p.y + bend}, ${q.x} ${q.y - bend}, ${q.x} ${q.y}`, class: target ? 'graph-edge' : 'graph-edge boundary' }));
    }
  }
  for (const commit of commits) {
    const p = positions.get(commit.sha);
    svg.appendChild(svgElement('circle', { cx: p.x, cy: p.y, r: 5, class: commit.sha === state.commit ? 'graph-node selected' : 'graph-node' }));
  }
  tree.appendChild(svg);
  const list = document.createElement('div');
  list.className = 'commit-rows';
  for (const commit of commits) {
    const row = document.createElement('div');
    row.className = 'commit-row' + (state.commit === commit.sha ? ' selected' : '');
    // Branch tips are labelled in the tree itself, before the message; the
    // commit id only appears on hover to keep the column narrow.
    const line = document.createElement('div'); line.className = 'commit-line';
    for (const branch of commit.branches || []) line.appendChild(chip(branch, 'branch'));
    const open = button(commit.message, 'commit-open', () => selectRepo(state.repoKey, commit.sha));
    open.title = commit.sha + '\n' + commit.message;
    if (commit.sha === state.commit) open.setAttribute('aria-current', 'true');
    line.appendChild(open);
    row.appendChild(line);
    const meta = document.createElement('div'); meta.className = 'row commit-meta';
    for (const result of commitVerdictChips(displayedRun(commit.sha, 'normal'), displayedRun(commit.sha, 'plan'))) {
      meta.appendChild(result);
    }
    const who = document.createElement('span'); who.className = 'note';
    who.textContent = [commit.author, commit.date ? timeAgo(commit.date) : ''].filter(Boolean).join(' · ');
    meta.appendChild(who);
    row.appendChild(meta);
    list.appendChild(row);
  }
  tree.appendChild(list);
}

// The intent field lives in the Plan card, which is rebuilt on every render, so
// keep a reference to the element itself: once detached, getElementById misses it.
let planIntentField = null;
const planIntent = () => (planIntentField ||= el('plan-intent-field')).querySelector('textarea');

function renderCommitActions(resetIntent = false) {
  const repo = state.repos.get(state.repoKey);
  if (!repo || !state.commit) return;
  el('commit-actions').classList.remove('hidden');
  const node = (state.graphs.get(repo.key)?.commits || []).find((c) => c.sha === state.commit);
  if (resetIntent) planIntent().value = cachedRun(state.commit, 'plan')?.intent || node?.message || '';
  // Rebuilding the cards detaches the textarea; restore focus if the user was typing.
  const intent = planIntent();
  const typing = document.activeElement === intent && [intent.selectionStart, intent.selectionEnd];
  const comparison = el('comparison'); comparison.textContent = '';
  for (const variant of ['normal', 'plan']) {
    const run = displayedRun(state.commit, variant);
    const card = document.createElement('div'); card.className = 'comparison-card';
    // Title, verdict and (for plans) the intent prompt share one line.
    const head = document.createElement('div'); head.className = 'card-head';
    const title = document.createElement('h3'); title.textContent = variant === 'plan' ? 'Plan' : 'Analysis'; head.appendChild(title);
    head.appendChild(verdictChip(run));
    if (variant === 'plan') {
      const label = document.createElement('label'); label.className = 'note'; label.htmlFor = 'plan-intent';
      label.textContent = 'Describe the task to see what impacts where planned';
      label.title = 'Editable; the plan starts from this commit’s first parent';
      head.appendChild(label);
    }
    card.appendChild(head);
    if (run) {
      const detail = document.createElement('p'); detail.className = 'note';
      detail.textContent = [analysisModeLabel(run.mode), run.base_commit ? 'Base ' + shortSha(run.base_commit) : '', run.finished_at ? timeAgo(run.finished_at) : '', run.error].filter(Boolean).join(' · ');
      card.appendChild(detail);
    }
    if (variant === 'plan') card.appendChild(planIntentField);
    const actions = document.createElement('div'); actions.className = 'row';
    const launch = button('Run ' + (variant === 'plan' ? 'plan' : 'analysis'),'btn small', () => analyzeCommit(variant));
    launch.disabled = run && ['queued', 'running'].includes(run.status);
    actions.appendChild(launch);
    const view = button('View cached result', 'btn quiet small', () => openReportDialog(variant));
    view.dataset.reportVariant = variant;
    view.setAttribute('aria-haspopup', 'dialog');
    view.setAttribute('aria-controls', 'report-dialog');
    view.disabled = !cachedRun(state.commit, variant);
    actions.appendChild(view);
    card.appendChild(actions);
    comparison.appendChild(card);
  }
  if (typing) { intent.focus(); intent.setSelectionRange(...typing); }
}

async function analyzeCommit(variant) {
  const repoKey = state.repoKey, commit = state.commit;
  const key = pendingKey(repoKey, commit, variant);
  const intent = planIntent().value.trim();
  if (variant === 'plan' && !intent) { toast('Enter an intent for the plan.', true); return; }
  // Shown until the hub names the attempt; events or polling then take over.
  const optimistic = { status: 'queued', commit, variant, repo_key: repoKey, requested: Date.now() };
  state.pending.set(key, optimistic);
  watchPending();
  renderCommitActions(); renderGraph();
  try {
    const queued = await api('/api/repos/' + encodeURIComponent(repoKey) + '/analyze', {
      method: 'POST', body: { commit, variant, intent: variant === 'plan' ? intent : '' },
    });
    if (queued?.commit === commit && state.pending.get(key) === optimistic) state.pending.delete(key);
    if (!followQueued(repoKey, queued) && repoKey === state.repoKey) { renderCommitActions(); renderGraph(); }
    toast((variant === 'plan' ? 'Plan' : 'Analysis') + ' queued for ' + shortSha(commit) + '.');
  } catch (err) {
    if (state.pending.get(key) === optimistic) state.pending.delete(key);
    if (repoKey === state.repoKey) { renderCommitActions(); renderGraph(); }
    toast(err.message, true);
  }
}

function analysisModeLabel(mode) {
  if (mode === 'review-read-only') return 'AI review (read-only)';
  return mode || 'lint';
}

function renderReportCommit(repo, run) {
  const commit = run?.commit || state.commit;
  if (!commit) return;
  const node = (state.graphs.get(repo?.key)?.commits || []).find((c) => c.sha === commit);
  const message = run?.message || node?.message;
  const line = document.createElement('div');
  line.className = 'report-commit-line';
  const heading = document.createElement('p');
  heading.id = 'selected-commit';
  heading.className = 'report-commit';
  heading.textContent = shortSha(commit) + (message ? ' · ' + message : '');
  line.appendChild(heading);
  if (repo?.web_url) {
    const forgeLink = document.createElement('a');
    forgeLink.className = 'btn quiet small';
    forgeLink.href = commitURL(repo, commit);
    forgeLink.target = '_blank';
    forgeLink.rel = 'noopener noreferrer';
    forgeLink.textContent = 'Open the commit';
    line.appendChild(forgeLink);
  }
  el('report-head').appendChild(line);
}

function clearReport(repo) {
  state.view = null; state.run = null; state.plan = null;
  el('report-head').textContent = '';
  const title = document.createElement('h2'); title.id = 'report-repo'; title.textContent = repo?.full_name || 'Select a repository';
  el('report-head').appendChild(title);
  renderReportCommit(repo);
  const sub = document.createElement('p'); sub.id = 'report-sub'; sub.className = 'report-sub'; el('report-head').appendChild(sub);
  el('filters').classList.add('hidden');
  el('alerts').textContent = '';
  el('extras').classList.add('hidden');
  el('plan-result').classList.add('hidden');
  el('report-empty').classList.remove('hidden');
  el('report-empty').textContent = 'No cached result for this mode. Launch an analysis above.';
}

function openReportDialog(variant) {
  state.variant = variant;
  const dialog = el('report-dialog');
  // Move the live report, preserving its controls and event listeners. The
  // dialog itself is outside the panels rebuilt by history and SSE updates.
  el('report-dialog-body').appendChild(el('report-pane'));
  if (!dialog.open) dialog.showModal();
  el('report-dialog-close').focus();
  loadReport();
}

function closeReportDialog() {
  const dialog = el('report-dialog');
  if (!dialog.open) return;
  dialog.close();
  el('splitter').after(el('report-pane'));
  // Live updates may have replaced the button that originally opened it.
  document.querySelector('[data-report-variant="' + state.variant + '"]')?.focus();
}

async function loadReport() {
  const repo = state.repos.get(state.repoKey);
  const commit = state.commit, variant = state.variant;
  if (!repo || !commit) return;
  clearReport(repo);
  const reportID = ++state.reportID;
  const run = cachedRun(commit, variant);
  if (!run) return;
  el('report-empty').textContent = 'Loading cached result…';
  try {
    const payload = await api('/api/repos/' + encodeURIComponent(repo.key)
      + '/reports/' + encodeURIComponent(commit) + '?variant=' + variant);
    if (reportID !== state.reportID || repo.key !== state.repoKey || commit !== state.commit || variant !== state.variant) return;
    if (variant === 'plan') { state.plan = payload; renderPlan(payload); return; }
    state.view = payload.view;
    state.run = payload.run;
    renderReport();
  } catch (err) {
    if (reportID !== state.reportID || repo.key !== state.repoKey || commit !== state.commit || variant !== state.variant) return;
    el('report-empty').textContent = run.error || err.message;
  }
}

function renderPlan(payload) {
  el('report-empty').classList.add('hidden');
  const holder = el('plan-result'); holder.textContent = ''; holder.classList.remove('hidden');
  const title = document.createElement('h3'); title.textContent = 'Cached plan · ' + shortSha(payload.run.commit); holder.appendChild(title);
  holder.appendChild(verdictChip(payload.run));
  const note = document.createElement('p'); note.className = 'note';
  note.textContent = 'Model-written proposal, not evidence. This plan starts at ' + shortSha(payload.run.base_commit) + '. It does not check the actual commit or approve it.';
  holder.appendChild(note);
  const pre = document.createElement('pre'); pre.className = 'policy'; pre.textContent = JSON.stringify(payload.plan, null, 2); holder.appendChild(pre);
  const link = document.createElement('a'); link.className = 'btn quiet small'; link.textContent = 'Download plan JSON';
  link.href = '/api/repos/' + encodeURIComponent(state.repoKey) + '/reports/' + encodeURIComponent(state.commit) + '/raw?variant=plan';
  link.download = 'PLAN.json'; holder.appendChild(link);
}

function renderReport() {
  const repo = state.repos.get(state.repoKey);
  const view = state.view;
  const run = state.run;
  if (!repo || !view) return;

  el('report-empty').classList.add('hidden');
  el('filters').classList.remove('hidden');

  const head = el('report-head');
  head.textContent = '';

  const title = document.createElement('div');
  title.className = 'report-title';
  const h2 = document.createElement('h2');
  h2.id = 'report-repo';
  h2.textContent = repo.full_name;
  title.appendChild(h2);
  const verdict = document.createElement('span');
  verdict.className = 'verdict ' + (view.summary.verdict || 'failed');
  if (view.summary.verdict === 'review') verdict.classList.add(reviewTone(view.summary));
  verdict.textContent = verdictLabel(view.summary.verdict);
  if (view.summary.verdict === 'review' && belowThreshold(view.summary)) {
    verdict.className = 'verdict below-threshold';
    verdict.textContent = 'Review below ' + LEVELS[state.minSeverity];
    verdict.title = 'Human review was requested at ' + reviewLevel(view.summary) + ' level, under the selected threshold.';
  }
  title.appendChild(verdict);
  if (run && run.status === 'failed') title.appendChild(chip('analysis failed', 'bad'));
  head.appendChild(title);

  renderReportCommit(repo, run);

  const sub = document.createElement('p');
  sub.className = 'report-sub';
  const parts = [];
  if (run) {
    if (run.author) parts.push('by ' + run.author);
    if (run.ref) parts.push(run.ref.replace('refs/heads/', ''));
    if (run.finished_at) parts.push(timeAgo(run.finished_at));
    if (run.trigger) parts.push(run.trigger);
  }
  sub.textContent = parts.join(' · ');
  head.appendChild(sub);

  if (run?.mode === 'review-read-only') {
    const note = document.createElement('p');
    note.className = 'report-sub';
    note.textContent = 'Read-only AI review: no code or tests were executed. Model suspicions are unverified hypotheses, not reproduced issues.';
    head.appendChild(note);
  }

  if (view.reviewer_summary) {
    const summary = document.createElement('div');
    summary.className = 'reviewer-summary';
    const label = document.createElement('b');
    label.textContent = 'AI reviewer summary';
    summary.appendChild(label);
    const text = document.createElement('p');
    text.textContent = view.reviewer_summary;
    summary.appendChild(text);
    const caveat = document.createElement('span');
    caveat.className = 'note';
    caveat.textContent = 'Model output, not evidence.';
    summary.appendChild(caveat);
    head.appendChild(summary);
  }

  if (run && run.error) {
    const error = document.createElement('p');
    error.className = 'report-sub';
    error.textContent = 'Error: ' + run.error;
    head.appendChild(error);
  }

  const stats = document.createElement('div');
  stats.className = 'stats';
  const s = view.summary;
  stats.appendChild(stat(s.counts.total, 'alerts'));
  stats.appendChild(stat(s.reproduced, 'reproduced'));
  stats.appendChild(stat(s.unverified, 'unverified'));
  if (s.suspicions) stats.appendChild(stat(s.suspicions, 'AI suspicions'));
  if (s.dismissed) stats.appendChild(stat(s.dismissed, 'set aside by AI'));
  stats.appendChild(stat(s.focused_lines + ' / ' + s.changed_lines, 'focused lines'));
  stats.appendChild(stat(s.changed_files, 'files'));
  stats.appendChild(stat('+' + s.additions + ' / -' + s.deletions, 'lines'));
  if (s.checks_passed + s.checks_failed > 0) {
    stats.appendChild(stat(s.checks_passed + ' / ' + (s.checks_passed + s.checks_failed), 'checks passed'));
  }
  head.appendChild(stats);

  el('download').href = '/api/repos/' + encodeURIComponent(repo.key)
    + '/reports/' + encodeURIComponent(state.commit) + '/raw';

  renderKindFilter();
  renderAlerts();
  renderExtras();
}

function verdictLabel(verdict) {
  switch (verdict) {
    case 'blocked': return 'Reproduced issue';
    case 'review': return 'Human review required';
    case 'clear': return 'No reproduced blocker';
    default: return 'Incomplete';
  }
}

function commitURL(repo, commit) {
  if (!repo.web_url) return '#';
  const separator = repo.provider === 'gitlab' ? '/-/commit/' : '/commit/';
  return repo.web_url.replace(/\/$/, '') + separator + commit;
}

function stat(value, label) {
  const box = document.createElement('div');
  box.className = 'stat';
  const b = document.createElement('b');
  b.textContent = String(value);
  const span = document.createElement('span');
  span.textContent = label;
  box.appendChild(b);
  box.appendChild(span);
  return box;
}

function renderKindFilter() {
  const holder = el('kinds');
  holder.textContent = '';
  const alerts = state.view ? state.view.alerts || [] : [];
  for (const kind of KINDS) {
    const count = kind.key === 'all'
      ? alerts.length
      : alerts.filter((a) => a.kind === kind.key).length;
    if (kind.key !== 'all' && count === 0) continue;
    const b = button(kind.label + ' (' + count + ')', state.kind === kind.key ? 'on' : '', () => {
      state.kind = kind.key;
      renderKindFilter();
      renderAlerts();
    });
    holder.appendChild(b);
  }
}

function filteredAlerts() {
  if (!state.view) return [];
  return (state.view.alerts || []).filter((alert) => {
    if (LEVELS.indexOf(alert.severity) < state.minSeverity) return false;
    if (state.kind !== 'all' && alert.kind !== state.kind) return false;
    return true;
  });
}

// loadMinSeverity restores the shared review threshold, defaulting to low.
function loadMinSeverity() {
  let saved = 0;
  try { saved = Number(localStorage.getItem(SEVERITY_KEY) || 0); } catch (err) { /* storage disabled */ }
  return Number.isInteger(saved) && saved >= 0 && saved < LEVELS.length ? saved : 0;
}

function loadPeriod() {
  let saved = NaN;
  try {
    const raw = localStorage.getItem(PERIOD_KEY);
    if (raw !== null) saved = Number(raw);
  } catch (err) { /* storage disabled */ }
  return Number.isInteger(saved) && saved >= 0 && saved < PERIODS.length ? saved : 3; // default: 1d
}

// renderPeriod keeps the period slider and its accessible value in sync.
function renderPeriod() {
  const label = PERIODS[state.period].label;
  el('period').value = String(state.period);
  el('period').setAttribute('aria-valuetext', label);
  el('period-value').textContent = label;
}

// setPeriod changes the aggregation period of every repository.
function setPeriod(index) {
  state.period = index;
  renderPeriod();
  try { localStorage.setItem(PERIOD_KEY, String(index)); } catch (err) { /* storage disabled */ }
  renderDashboard();
}

// renderSeverity keeps the slider and its accessible value in sync with the preference.
function renderSeverity() {
  el('severity').value = String(state.minSeverity);
  el('severity').setAttribute('aria-valuetext', LEVELS[state.minSeverity]);
  el('severity-value').textContent = LEVELS[state.minSeverity];
}

// setMinSeverity changes the threshold for every repository and redraws each
// place that flags a review: the list, the tree, the commit cards and the report.
function setMinSeverity(level) {
  state.minSeverity = level;
  renderSeverity();
  try { localStorage.setItem(SEVERITY_KEY, String(level)); } catch (err) { /* storage disabled */ }
  renderDashboard();
}

function renderDashboard() {
  renderRepos();
  renderGraph();
  renderCommitActions();
  if (state.variant === 'plan') {
    if (state.plan && state.plan.run && state.plan.run.commit === state.commit) renderPlan(state.plan);
  } else if (state.view) {
    renderReport();
  }
}

function renderAlerts() {
  const list = el('alerts');
  list.textContent = '';
  const alerts = filteredAlerts();
  const total = state.view ? (state.view.alerts || []).length : 0;
  el('alert-count').textContent = alerts.length + ' of ' + total + ' alerts shown';

  if (alerts.length === 0) {
    const empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = total === 0
      ? 'This run recorded no alert.'
      : 'No alert at this severity. Lower the filter to see the rest.';
    list.appendChild(empty);
    return;
  }

  for (const alert of alerts) list.appendChild(alertItem(alert, renderAlerts));
}

// alertItem renders one alert, folded or unfolded; rerender redraws the list
// that holds it after a click.
function alertItem(alert, rerender) {
  const item = document.createElement('li');
  item.className = 'alert';

  const head = document.createElement('button');
  head.className = 'alert-head';
  head.setAttribute('aria-expanded', state.expanded.has(alert.id) ? 'true' : 'false');

  const dot = document.createElement('span');
  dot.className = 'dot ' + severityClass(alert.severity);
  head.appendChild(dot);

  const middle = document.createElement('span');
  const title = document.createElement('div');
  title.className = 'alert-title';
  title.textContent = alert.title || alert.id;
  middle.appendChild(title);
  const where = document.createElement('div');
  where.className = 'alert-where';
  where.textContent = alertLocation(alert);
  middle.appendChild(where);
  head.appendChild(middle);

  const tags = document.createElement('span');
  tags.className = 'alert-tags';
  tags.appendChild(dotChip(alert.severity, alert.severity));
  if (alert.original_severity) {
    // The CLI lowered it one level after the AI read the signal as harmless.
    const lowered = chip('was ' + alert.original_severity, 'lowered');
    lowered.title = 'The AI reviewer read this signal as harmless: its severity was lowered from ' + alert.original_severity + ' to ' + alert.severity + '.';
    tags.appendChild(lowered);
  }
  tags.appendChild(chip(alert.kind));
  if (alert.status) tags.appendChild(chip(alert.status.toLowerCase(), statusClass(alert.status)));
  if (alert.judgment) tags.appendChild(chip('AI: ' + judgmentLabel(alert.judgment), judgmentClass(alert.judgment)));
  head.appendChild(tags);

  head.addEventListener('click', () => {
    if (state.expanded.has(alert.id)) state.expanded.delete(alert.id);
    else state.expanded.add(alert.id);
    rerender();
  });
  item.appendChild(head);

  if (state.expanded.has(alert.id)) {
    item.appendChild(alertBody(alert));
  }
  return item;
}

// The reviewer model's reading of a linter signal: model judgment, never a verdict.
function judgmentLabel(judgment) {
  switch (judgment) {
    case 'risk': return 'risk';
    case 'no_risk': return 'no risk';
    default: return 'uncertain';
  }
}

function judgmentClass(judgment) {
  switch (judgment) {
    case 'risk': return 'bad';
    case 'no_risk': return 'ok';
    default: return 'warn';
  }
}

function statusClass(status) {
  switch (String(status).toUpperCase()) {
    case 'REPRODUCED': return 'bad';
    case 'UNVERIFIED': return 'warn';
    case 'NOT_REPRODUCED':
    case 'DISMISSED':
    case 'PASS': return 'ok';
    case 'FAIL':
    case 'ERROR':
    case 'TIMEOUT': return 'bad';
    default: return '';
  }
}

function alertLocation(alert) {
  if (alert.members && alert.members.length) {
    const paths = [...new Set(alert.members.map((member) => member.path))];
    return (paths.length === 1 ? paths[0] + ' · whole file' : paths.length + ' files')
      + ' · ' + alert.members.length + ' signals grouped';
  }
  if (!alert.path) return alert.reasons ? alert.reasons.join(', ') : '';
  if (alert.scope === 'file') return alert.path + ' · whole file';
  let where = alert.path;
  if (alert.line) {
    where += ':' + alert.line;
    if (alert.end_line && alert.end_line !== alert.line) where += '-' + alert.end_line;
  }
  if (alert.side && alert.side !== 'new') where += ' (' + alert.side + ' side)';
  return where;
}

// alertBody shows the rationale, the recorded evidence and, above all, the
// modifications the alert concerns.
function alertBody(alert, showDiff = true) {
  const body = document.createElement('div');
  body.className = 'alert-body';

  if (alert.members && alert.members.length) {
    const files = new Map();
    for (const member of alert.members) {
      if (!files.has(member.path)) files.set(member.path, []);
      files.get(member.path).push(member);
    }
    for (const [path, members] of files) {
      const section = document.createElement('div');
      section.className = 'alert-group-file';
      const label = document.createElement('h4');
      label.textContent = path;
      section.appendChild(label);
      const shown = new Set();
      for (const member of members) {
        const { id, ...content } = member;
        const key = JSON.stringify(content);
        if (shown.has(key)) continue;
        shown.add(key);
        const title = document.createElement('b');
        title.textContent = member.title;
        section.appendChild(title);
        section.appendChild(alertBody(member, false));
      }
      appendAlertDiff(section, members[0]);
      body.appendChild(section);
    }
    return body;
  }

  if (alert.explanation || alert.rationale && alert.judgment) {
    const reading = document.createElement('div');
    reading.className = 'ai-reading';
    const label = document.createElement('b');
    label.textContent = 'AI reading (' + judgmentLabel(alert.judgment) + ')';
    reading.appendChild(label);
    if (alert.explanation) {
      const explanation = document.createElement('p');
      explanation.textContent = alert.explanation;
      reading.appendChild(explanation);
    }
    if (alert.rationale) {
      const rationale = document.createElement('p');
      rationale.className = 'note';
      rationale.textContent = 'Why: ' + alert.rationale;
      reading.appendChild(rationale);
    }
    body.appendChild(reading);
  }
  if (alert.original_title) {
    const original = document.createElement('div');
    original.className = 'note';
    original.textContent = 'Linter: ' + alert.original_title;
    body.appendChild(original);
  }
  if (alert.detail) {
    const detail = document.createElement('div');
    detail.className = 'alert-detail';
    detail.textContent = alert.detail;
    body.appendChild(detail);
  }
  if (alert.reasons && alert.reasons.length > 0) {
    const reasons = document.createElement('div');
    reasons.className = 'row';
    for (const reason of alert.reasons) reasons.appendChild(chip(reason));
    body.appendChild(reasons);
  }
  for (const evidence of alert.evidence || []) {
    const box = document.createElement('div');
    box.className = 'evidence';
    const head = document.createElement('div');
    head.className = 'row';
    head.appendChild(chip(evidence.kind));
    head.appendChild(chip(evidence.status.toLowerCase(), statusClass(evidence.status)));
    if (evidence.runner) head.appendChild(chip(evidence.runner));
    box.appendChild(head);
    const description = document.createElement('div');
    description.textContent = evidence.description || '';
    box.appendChild(description);
    if (evidence.test_names && evidence.test_names.length > 0) {
      const tests = document.createElement('div');
      tests.className = 'note mono';
      tests.textContent = 'tests: ' + evidence.test_names.join(', ');
      box.appendChild(tests);
    }
    if (evidence.output) {
      const output = document.createElement('pre');
      output.className = 'alert-detail mono';
      output.textContent = evidence.output;
      box.appendChild(output);
    }
    body.appendChild(box);
  }

  if (showDiff) appendAlertDiff(body, alert);
  return body;
}

function appendAlertDiff(body, alert) {
  const file = alert.path ? findFile(alert.path) : null;
  if (file) {
    body.appendChild(renderDiff(file, alert));
  } else if (alert.path) {
    const missing = document.createElement('p');
    missing.className = 'note';
    missing.textContent = alert.line
      ? alertLines(alert) + ' of ' + alert.path + ' outside the recorded diff: the analyzed range does not change this file.'
      : 'The analyzed range carries no diff for ' + alert.path + '.';
    body.appendChild(missing);
  }
}

// alertLines names the lines an alert points at, as the subject of a sentence.
function alertLines(alert) {
  const side = alert.side === 'old' ? ' (old side)' : '';
  if (alert.end_line && alert.end_line > alert.line) return 'Lines ' + alert.line + '-' + alert.end_line + side + ' are';
  return 'Line ' + alert.line + side + ' is';
}

// diffNote says why the diff singles out no line: the alert concerns the whole
// file, or the diff does not show the lines it points at.
function diffNote(alert, focused) {
  let text = '';
  if (alert.scope === 'file') text = 'This alert concerns the whole file, not one of its lines. All its modifications follow.';
  else if (alert.line && !focused) text = alertLines(alert) + ' outside the recorded diff. The modifications of this file follow.';
  if (!text) return null;
  const note = document.createElement('p');
  note.className = 'note diff-note';
  note.textContent = text;
  return note;
}

function findFile(path) {
  if (!state.view) return null;
  return (state.view.files || []).find((file) => file.path === path || file.old_path === path) || null;
}

// renderDiff shows every modification of the concerned file and highlights the
// lines the alert points at, or says why it highlights none.
function renderDiff(file, alert) {
  const wrapper = document.createElement('div');
  wrapper.className = 'diff';

  const header = document.createElement('div');
  header.className = 'diff-file';
  const path = document.createElement('span');
  path.className = 'path mono';
  path.textContent = file.path;
  header.appendChild(path);
  header.appendChild(chip(statusLabel(file.status)));
  header.appendChild(chip('+' + file.additions + ' / -' + file.deletions));
  if (file.old_path) header.appendChild(chip('was ' + file.old_path));
  wrapper.appendChild(header);

  if (file.binary || !file.hunks || file.hunks.length === 0) {
    const none = document.createElement('div');
    none.className = 'empty';
    none.textContent = file.binary ? 'Binary file: no line-level diff.' : 'No hunk recorded for this file.';
    wrapper.appendChild(none);
    return wrapper;
  }

  const table = document.createElement('table');
  const body = document.createElement('tbody');
  let firstFocus = null;
  for (const hunk of file.hunks) {
    const headerRow = document.createElement('tr');
    headerRow.className = 'hunk-header';
    const cell = document.createElement('td');
    cell.colSpan = 4;
    cell.className = 'mono';
    cell.textContent = '@@ -' + hunk.old_start + ',' + hunk.old_lines
      + ' +' + hunk.new_start + ',' + hunk.new_lines + ' @@';
    headerRow.appendChild(cell);
    body.appendChild(headerRow);

    for (const line of hunk.lines || []) {
      const row = document.createElement('tr');
      row.className = line.kind === 'add' ? 'add' : (line.kind === 'delete' ? 'del' : 'ctx');
      if (inAlertRange(line, alert)) {
        row.classList.add('focus');
        if (!firstFocus) firstFocus = row;
      }
      row.appendChild(cellText(line.old_line || '', 'num mono'));
      row.appendChild(cellText(line.new_line || '', 'num mono'));
      row.appendChild(cellText(line.kind === 'add' ? '+' : (line.kind === 'delete' ? '-' : ' '), 'sign mono'));
      row.appendChild(cellText(line.content, 'mono'));
      body.appendChild(row);
    }
  }
  table.appendChild(body);

  const note = diffNote(alert, firstFocus);
  if (note) wrapper.appendChild(note);
  const pre = document.createElement('div');
  pre.appendChild(table);
  wrapper.appendChild(pre);

  if (firstFocus) {
    // Bring the flagged lines into view without stealing the page scroll.
    requestAnimationFrame(() => firstFocus.scrollIntoView({ block: 'center' }));
  }
  return wrapper;
}

function statusLabel(status) {
  switch (status) {
    case 'A': return 'added';
    case 'D': return 'deleted';
    case 'M': return 'modified';
    case 'R': return 'renamed';
    case 'C': return 'copied';
    case 'T': return 'type changed';
    default: return status || 'changed';
  }
}

function inAlertRange(line, alert) {
  if (!alert || !alert.line) return false;
  const side = alert.side === 'old' ? 'old_line' : 'new_line';
  const number = line[side];
  if (!number) return false;
  const end = alert.end_line && alert.end_line >= alert.line ? alert.end_line : alert.line;
  return number >= alert.line && number <= end;
}

function cellText(text, className) {
  const cell = document.createElement('td');
  cell.className = className;
  cell.textContent = String(text);
  return cell;
}

// renderExtras shows what is deliberately not an alert: coverage, the review
// surface, and everything the run left unverified.
function renderExtras() {
  const holder = el('extras');
  holder.textContent = '';
  const view = state.view;
  if (!view) { holder.classList.add('hidden'); return; }
  holder.classList.remove('hidden');

  const dismissed = view.dismissed || [];
  if (dismissed.length > 0) {
    // What the AI reviewer set aside stays one click away: it is model
    // judgment, recorded by the CLI (ai_impacts_criticality).
    const box = document.createElement('details');
    box.className = 'dismissed';
    box.open = state.showDismissed;
    box.addEventListener('toggle', () => { state.showDismissed = box.open; });
    const summary = document.createElement('summary');
    summary.textContent = 'Set aside by the AI reviewer (' + dismissed.length + ')';
    box.appendChild(summary);
    const caveat = document.createElement('p');
    caveat.className = 'note';
    caveat.textContent = 'Low signals the AI reviewer read as harmless after reading the source, and hypotheses it dismissed. Model judgment, not evidence.';
    box.appendChild(caveat);
    const list = document.createElement('ul');
    list.className = 'alerts';
    for (const alert of dismissed) list.appendChild(alertItem(alert, renderExtras));
    box.appendChild(list);
    holder.appendChild(box);
  }

  const surface = document.createElement('p');
  surface.className = 'note';
  surface.textContent = view.review_surface && view.review_surface.note ? view.review_surface.note : '';
  if (surface.textContent) holder.appendChild(surface);

  if (view.coverage) {
    const coverage = document.createElement('div');
    coverage.className = 'row';
    coverage.appendChild(chip('changed-line execution: ' + view.coverage.status));
    if (view.coverage.status === 'measured') {
      coverage.appendChild(chip(view.coverage.executed_lines + ' executed'));
      coverage.appendChild(chip(view.coverage.not_executed_lines + ' not executed'));
      coverage.appendChild(chip(view.coverage.not_measured_lines + ' not measured'));
    } else if (view.coverage.reason) {
      coverage.appendChild(chip(view.coverage.reason));
    }
    holder.appendChild(coverage);
  }

  if (view.unverified && view.unverified.length > 0) {
    const title = document.createElement('p');
    title.className = 'note';
    title.textContent = 'Left unverified by this run:';
    holder.appendChild(title);
    const list = document.createElement('ul');
    for (const item of view.unverified) {
      const li = document.createElement('li');
      li.className = 'note';
      li.textContent = item;
      list.appendChild(li);
    }
    holder.appendChild(list);
  }

  if (view.diff_truncated) {
    const truncated = document.createElement('p');
    truncated.className = 'note';
    truncated.textContent = 'The diff of this change was too large to render in full; download the JSON report for everything.';
    holder.appendChild(truncated);
  }
}

/* ------------------------------------------------------------------ live -- */

function connectEvents() {
  const stream = new EventSource('/api/events');
  stream.onmessage = (message) => {
    let event;
    try { event = JSON.parse(message.data); } catch (err) { return; }
    if (event.type === 'run' || event.type === 'report') refreshActivity();
    if (event.type === 'repo' && event.repo) {
      const previous = state.repos.get(event.repo.key);
      if (previous && previous.recent_incomplete) event.repo.recent_incomplete = true;
      state.repos.set(event.repo.key, event.repo);
      // A repository snapshot may carry an older attempt than one already seen.
      if (!event.repo.latest || trackRun(event.repo.key, event.repo.latest)) rememberRun(event.repo.key, event.repo.latest);
      renderRepos();
      if (event.repo.key === state.repoKey) { renderGraph(); renderCommitActions(); }
    } else if (event.type === 'run') {
      const run = event.run;
      // A cancelled attempt has no result: reload the stored state it hid.
      if (run.status === 'cancelled') {
        trackRun(event.repo_key, run);
        refreshDashboard(event.repo_key === state.repoKey);
      } else if (trackRun(event.repo_key, run)) rememberRun(event.repo_key, run);
      renderRepos();
      if (event.repo_key === state.repoKey) { renderGraph(); renderCommitActions(); }
    } else if (event.type === 'report') {
      const run = Object.assign({ commit: event.commit }, event.run);
      if (trackRun(event.repo_key, run)) rememberRun(event.repo_key, run);
      renderRepos();
      const repo = state.repos.get(event.repo_key);
      if (event.repo_key === state.repoKey) {
        loadHistory();
      } else if (repo) {
        toast('New result for ' + repo.full_name + ' (' + shortSha(event.commit) + ').');
      }
    } else if (event.type === 'sync' && event.status === 'finished') {
      loadRepos();
      toast('Repository list refreshed (' + event.count + ').');
    } else if (event.type === 'sync' && event.status === 'failed') {
      toast(event.error || 'The repository sync failed.', true);
    }
  };
  let connected = false;
  stream.onopen = () => {
    refreshActivity();
    if (connected) refreshDashboard(true); // Recover events missed during a disconnect.
    connected = true;
  };
  stream.onerror = () => { /* EventSource retries on its own. */ };
}

/* ------------------------------------------------------------------ boot -- */

let reposLoadID = 0;
async function loadRepos() {
  const loadID = ++reposLoadID;
  const beforeRepos = new Map(state.repos);
  const beforeRuns = new Map(Array.from(state.recent, ([key, runs]) => [key, new Map(runs)]));
  const payload = await api('/api/repos');
  if (loadID !== reposLoadID) return;
  state.recentLimit = payload.recent_limit;
  // Preserve live updates received while the snapshot request was in flight.
  const liveRepos = Array.from(state.repos).filter(([key, repo]) => repo !== beforeRepos.get(key));
  const liveRuns = [];
  for (const [key, runs] of state.recent) {
    for (const [commit, run] of runs) {
      if (run !== beforeRuns.get(key)?.get(commit)) liveRuns.push([key, run]);
    }
  }
  state.repos = new Map((payload.repos || []).map((repo) => [repo.key, repo]));
  state.recent = new Map();
  for (const repo of state.repos.values()) {
    for (const run of repo.recent || []) {
      rememberRun(repo.key, mergeRecentRun(beforeRuns.get(repo.key)?.get(run.commit), run));
    }
    if (repo.latest) rememberRun(repo.key, mergeRecentRun(beforeRuns.get(repo.key)?.get(repo.latest.commit), repo.latest));
    delete repo.recent; // Avoid keeping a duplicate account-wide run cache.
  }
  for (const [key, repo] of liveRepos) state.repos.set(key, repo);
  for (const [key, run] of liveRuns) rememberRun(key, run);
  renderRepos();
}

let refreshingDashboard = false;
async function refreshDashboard(refreshHistory = false) {
  if (refreshingDashboard || document.hidden) return;
  refreshingDashboard = true;
  try {
    await loadRepos();
    if (refreshHistory && state.repoKey) await loadHistory();
    renderDashboard();
  } catch (err) {
    toast('Could not refresh results: ' + err.message, true);
    renderRepos(); // Still age the current snapshot while offline.
  } finally {
    refreshingDashboard = false;
  }
}

function readHash() {
  const match = /^#\/repo\/([^/]+)(?:\/commit\/([0-9a-f]+))?/.exec(window.location.hash || '');
  if (!match) return null;
  return { repoKey: decodeURIComponent(match[1]), commit: match[2] || null };
}

async function boot() {
  let me;
  try {
    me = await api('/api/me');
  } catch (err) {
    window.location.replace('/index.html');
    return;
  }
  if (!me.authenticated) {
    window.location.replace('/index.html');
    return;
  }
  state.me = me;
  state.csrf = me.csrf;
  el('mode-label').textContent = 'Hub ' + (me.version || '') + ' · ' + analysisModeLabel(me.mode);

  const who = el('who');
  if (me.user.avatar_url) {
    const avatar = document.createElement('img');
    avatar.src = me.user.avatar_url;
    avatar.alt = '';
    who.appendChild(avatar);
  }
  who.appendChild(document.createTextNode(me.user.login + ' · ' + me.user.provider));

  el('signout').addEventListener('click', async () => {
    try { await api('/auth/logout', { method: 'POST' }); } catch (err) { /* ignore */ }
    window.location.replace('/index.html');
  });
  el('analyses').addEventListener('click', openActivityDialog);
  el('sync').addEventListener('click', async (event) => {
    event.target.disabled = true;
    try {
      await api('/api/repos/sync', { method: 'POST' });
      toast('Refreshing the repository list…');
    } catch (err) {
      toast(err.message, true);
    }
    setTimeout(() => { event.target.disabled = false; }, 3000);
  });
  el('repo-search').addEventListener('input', (event) => {
    state.query = event.target.value;
    renderRepos();
  });
  restoreRepoFilters();
  el('only-monitored').addEventListener('change', (event) => {
    state.onlyMonitored = event.target.checked;
    saveRepoFilters();
    renderRepos();
  });
  el('only-policy').addEventListener('change', (event) => {
    state.onlyPolicy = event.target.checked;
    if (state.onlyPolicy) {
      state.onlyMissing = false;
      el('only-nopolicy').checked = false;
    }
    saveRepoFilters();
    renderRepos();
  });
  el('only-nopolicy').addEventListener('change', (event) => {
    state.onlyMissing = event.target.checked;
    if (state.onlyMissing) {
      state.onlyPolicy = false;
      el('only-policy').checked = false;
    }
    saveRepoFilters();
    renderRepos();
  });
  renderPeriod();
  el('period').addEventListener('input', (event) => setPeriod(Number(event.target.value)));
  renderSeverity();
  el('severity').addEventListener('input', (event) => setMinSeverity(Number(event.target.value)));
  // Keep sticky panels below a header that can wrap as controls are added.
  const topbar = document.querySelector('.topbar');
  const sizeTopbar = () => document.documentElement.style.setProperty('--panel-top', (topbar.offsetHeight + 8) + 'px');
  sizeTopbar();
  new ResizeObserver(sizeTopbar).observe(topbar);
  initSplitter();
  initPanelJumps();
  el('refresh-commits').addEventListener('click', () => {
    state.graphs.delete(state.repoKey);
    loadHistory();
  });
  el('modal-close').addEventListener('click', closeModal);
  el('report-dialog-close').addEventListener('click', closeReportDialog);
  el('report-dialog').addEventListener('cancel', (event) => {
    event.preventDefault();
    closeReportDialog();
  });
  el('modal').addEventListener('click', (event) => {
    if (event.target === el('modal')) closeModal();
  });
  document.addEventListener('keydown', (event) => {
    if (el('report-dialog').open) return; // Native modal handles focus and Escape.
    if (event.key === 'Escape') closeModal();
    if (event.key === 'Tab' && !el('modal').classList.contains('hidden')) {
      const focusable = [...el('modal').querySelectorAll('button:not(:disabled), input:not(:disabled), textarea:not(:disabled), a[href], select:not(:disabled)')];
      const first = focusable[0], last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    }
  });
  window.addEventListener('hashchange', () => {
    const route = readHash();
    if (route && (route.repoKey !== state.repoKey || route.commit !== state.commit)) {
      selectRepo(route.repoKey, route.commit);
    }
  });

  await loadRepos();
  connectEvents();
  setInterval(refreshDashboard, 60000);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshDashboard(true); });
  const route = readHash();
  if (route && state.repos.has(route.repoKey)) {
    selectRepo(route.repoKey, route.commit);
  }
  if (state.repos.size === 0) {
    // A fresh account has nothing yet; the sign-in already started a sync.
    toast('Listing your repositories…');
  }
}

// The commit tree column can be resized by dragging (or with the arrow keys
// on) the separator; the width is remembered across visits.
const SPLIT_KEY = 'probe.commitColumnWidth';
const SPLIT_DEFAULT = 340;
const SPLIT_MIN = 220;

function clampSplit(width) {
  const workspace = document.querySelector('.workspace');
  const max = Math.max(SPLIT_MIN, (workspace ? workspace.clientWidth : window.innerWidth) - 360);
  return Math.round(Math.min(max, Math.max(SPLIT_MIN, width)));
}

function setSplit(width, persist) {
  const value = clampSplit(width);
  const splitter = el('splitter');
  document.documentElement.style.setProperty('--commit-column-width', value + 'px');
  splitter.setAttribute('aria-valuenow', String(value));
  if (persist) {
    try { localStorage.setItem(SPLIT_KEY, String(value)); } catch (err) { /* storage disabled */ }
  }
  return value;
}

function initSplitter() {
  const splitter = el('splitter');
  const column = el('commit-browser');
  let saved = NaN;
  try { saved = Number(localStorage.getItem(SPLIT_KEY)); } catch (err) { /* storage disabled */ }
  setSplit(saved > 0 ? saved : SPLIT_DEFAULT, false);

  splitter.addEventListener('pointerdown', (event) => {
    if (event.button !== 0) return;
    event.preventDefault();
    const startX = event.clientX;
    const startWidth = column.getBoundingClientRect().width;
    try { splitter.setPointerCapture(event.pointerId); } catch (err) { /* synthetic pointer */ }
    splitter.classList.add('dragging');
    document.body.classList.add('resizing');
    const move = (e) => setSplit(startWidth + e.clientX - startX, false);
    const stop = (e) => {
      splitter.removeEventListener('pointermove', move);
      splitter.removeEventListener('pointerup', stop);
      splitter.removeEventListener('pointercancel', stop);
      splitter.classList.remove('dragging');
      document.body.classList.remove('resizing');
      setSplit(startWidth + e.clientX - startX, true);
    };
    splitter.addEventListener('pointermove', move);
    splitter.addEventListener('pointerup', stop);
    splitter.addEventListener('pointercancel', stop);
  });
  splitter.addEventListener('dblclick', () => setSplit(SPLIT_DEFAULT, true));
  splitter.addEventListener('keydown', (event) => {
    const step = event.shiftKey ? 80 : 20;
    const current = column.getBoundingClientRect().width;
    if (event.key === 'ArrowLeft') setSplit(current - step, true);
    else if (event.key === 'ArrowRight') setSplit(current + step, true);
    else if (event.key === 'Home') setSplit(SPLIT_MIN, true);
    else if (event.key === 'End') setSplit(Number.MAX_SAFE_INTEGER, true);
    else return;
    event.preventDefault();
  });
  window.addEventListener('resize', () => {
    let preferred = NaN;
    try { preferred = Number(localStorage.getItem(SPLIT_KEY)); } catch (err) { /* storage disabled */ }
    setSplit(preferred > 0 ? preferred : SPLIT_DEFAULT, false);
  });
}

// Scrolls a stacked (mobile) panel just under the sticky top bar.
function scrollToPanel(target) {
  const offset = document.querySelector('.topbar')?.offsetHeight || 0;
  const top = target.getBoundingClientRect().top + window.scrollY - offset - 8;
  const smooth = !window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  window.scrollTo({ top: Math.max(0, top), behavior: smooth ? 'smooth' : 'auto' });
}

function initPanelJumps() {
  // The repository panel leads to the commit tree once a repository is open,
  // otherwise to the report panel; the commit tree leads to the report.
  el('jump-from-repos').addEventListener('click', () => {
    const tree = el('commit-browser');
    scrollToPanel(tree.classList.contains('hidden') ? el('report-panel') : tree);
  });
  el('jump-from-commits').addEventListener('click', () => scrollToPanel(el('report-pane')));
}

document.addEventListener('DOMContentLoaded', boot);
