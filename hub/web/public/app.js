// SwiftProof Hub dashboard.
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
const SEVERITY_KEY = 'swiftproof.hub.minSeverity';

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
const PERIOD_KEY = 'swiftproof.hub.period';

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
  pending: new Map(),
  loadID: 0,
  reportID: 0,
  minSeverity: loadMinSeverity(),
  period: loadPeriod(),
  // Recent normal runs per repository key, keyed by commit, kept up to date
  // by the live events.
  recent: new Map(),
  plan: null,
  kind: 'all',
  query: '',
  onlyMonitored: false,
  onlyMissing: false,
  expanded: new Set(),
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
    init.headers['X-SwiftProof-CSRF'] = state.csrf;
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

// rememberRun keeps a normal run in the repository's recent cache.
function rememberRun(repoKey, run) {
  if (!run || !run.commit || (run.variant && run.variant !== 'normal')) return;
  if (!state.recent.has(repoKey)) state.recent.set(repoKey, new Map());
  const runs = state.recent.get(repoKey);
  // An update keeps the fields it does not carry, such as the queue time.
  runs.set(run.commit, Object.assign({}, runs.get(run.commit), run));
}

// periodRuns lists the runs of a repository queued within the selected period.
function periodRuns(repo) {
  const since = Date.now() - PERIODS[state.period].hours * 3600 * 1000;
  const runs = state.recent.get(repo.key);
  if (!runs) return [];
  return Array.from(runs.values()).filter((run) => {
    const at = new Date(run.queued_at || run.finished_at || 0).getTime();
    return !Number.isNaN(at) && at >= since;
  });
}

// worstRun returns the most severe run of the period; among equals, the newest.
function worstRun(repo) {
  let worst = null;
  for (const run of periodRuns(repo)) {
    if (!worst || statusRank(run) > statusRank(worst) ||
        (statusRank(run) === statusRank(worst) && (run.queued_at || '') > (worst.queued_at || ''))) {
      worst = run;
    }
  }
  return worst;
}

function visibleRepos() {
  const query = state.query.trim().toLowerCase();
  return Array.from(state.repos.values()).filter((repo) => {
    if (query && !repo.full_name.toLowerCase().includes(query)) return false;
    if (state.onlyMonitored && !repo.monitored) return false;
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
    } else {
      const none = chip('no commit in ' + period, 'unknown');
      none.title = 'No analysis in the selected period';
      meta.appendChild(none);
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
  const repos = Array.from(state.repos.values()).filter((repo) => periodRuns(repo).some(needsReview)).length;
  const parts = [repos + (repos === 1 ? ' repository' : ' repositories')];
  if (state.repoKey && state.graphs.has(state.repoKey)) {
    const commits = new Set(state.runs.filter(needsReview).map((run) => run.commit)).size;
    parts.push(commits + (commits === 1 ? ' commit' : ' commits'));
  }
  const count = el('review-count');
  count.textContent = parts.join(' · ') + ' to review';
  count.classList.toggle('warn', repos > 0);
}

// repoActions builds the buttons shown to the right of a repository name.
// A repository that is not monitored yet gets a single "Activate monitoring"
// action: it commits a .swiftproof.json policy first when there is none.
function repoActions(repo) {
  const actions = document.createElement('div');
  actions.className = 'repo-actions';
  const noAdmin = 'Your account cannot manage webhooks on this repository';
  if (!repo.has_policy) {
    const activate = button('Activate monitoring', 'btn setup small', (event) => {
      event.stopPropagation();
      openPolicyDialog(repo);
    });
    activate.title = 'Create a .swiftproof.json policy, then watch new commits';
    actions.appendChild(activate);
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
  const body = el('modal-body');
  const footer = el('modal-footer');
  el('modal-title').textContent = 'Activate monitoring on ' + repo.full_name;
  body.textContent = '';
  footer.textContent = '';

  const intro = document.createElement('p');
  intro.className = 'note';
  intro.textContent = 'Monitoring needs a .swiftproof.json policy. It is generated by the SwiftProof CLI this service runs, and committed on '
    + (repo.default_branch || 'the default branch')
    + '. Review the sandbox image and the commands before relying on a report.';
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

  const create = button(repo.admin ? 'Commit the policy and monitor' : 'Commit the policy', 'btn', async () => {
    create.disabled = true;
    try {
      const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/policy', {
        method: 'POST',
        body: { language: select.value === 'auto' ? '' : select.value },
      });
      upsertRepo(payload.repo);
      closeModal();
      toast('Committed .swiftproof.json on ' + repo.default_branch + '.');
      // The policy was only the first step of "Activate monitoring".
      const updated = payload.repo || repo;
      if (updated.admin) await setMonitoring(updated, true);
      else toast('Your account cannot manage webhooks on this repository: ask an administrator to activate monitoring.', true);
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

function closeModal() { el('modal').classList.add('hidden'); }

async function setMonitoring(repo, on, trigger) {
  if (trigger) trigger.disabled = true;
  try {
    const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/monitor', {
      method: on ? 'POST' : 'DELETE',
    });
    upsertRepo(payload.repo);
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
    await api('/api/repos/' + encodeURIComponent(repo.key) + '/analyze', { method: 'POST', body: {} });
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
    api('/api/repos/' + encodeURIComponent(repo.key) + '/runs?limit=0'),
  ]);
  if (loadID !== state.loadID || repo.key !== state.repoKey) return;
  const [graphResult, runsResult] = results;
  if (runsResult.status === 'fulfilled') state.runs = runsResult.value.runs || [];
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

function pendingKey(repoKey, commit, variant) { return repoKey + '/' + commit + '/' + variant; }

function displayedRun(commit, variant) {
  return state.pending.get(pendingKey(state.repoKey, commit, variant)) || cachedRun(commit, variant);
}

// Combine review requests in the tree, keeping the most severe report's
// presentation. Other statuses remain visible for each variant.
function commitVerdictChips(normal, plan) {
  if ([normal, plan].every((run) => run && run.status === 'done' && run.summary?.verdict === 'review')) {
    const result = verdictChip(statusRank(plan) > statusRank(normal) ? plan : normal);
    result.title = ['Analysis: ' + reviewLevel(normal.summary), 'Plan: ' + reviewLevel(plan.summary), result.title].filter(Boolean).join('\n');
    return [result];
  }
  const planChip = verdictChip(plan);
  planChip.prepend(document.createTextNode('Plan: '));
  return [verdictChip(normal), planChip];
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
      detail.textContent = [run.mode, run.base_commit ? 'Base ' + shortSha(run.base_commit) : '', run.finished_at ? timeAgo(run.finished_at) : '', run.error].filter(Boolean).join(' · ');
      card.appendChild(detail);
    }
    if (variant === 'plan') card.appendChild(planIntentField);
    const actions = document.createElement('div'); actions.className = 'row';
    const launch = button('Run ' + (variant === 'plan' ? 'plan' : 'analysis'),'btn small', () => analyzeCommit(variant));
    launch.disabled = run && ['queued', 'running'].includes(run.status);
    actions.appendChild(launch);
    const view = button('View cached result', 'btn quiet small', () => { state.variant = variant; loadReport(); });
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
  state.pending.set(key, { status: 'queued', variant });
  renderCommitActions(); renderGraph();
  try {
    await api('/api/repos/' + encodeURIComponent(repoKey) + '/analyze', {
      method: 'POST', body: { commit, variant, intent: variant === 'plan' ? intent : '' },
    });
    toast((variant === 'plan' ? 'Plan' : 'Analysis') + ' queued for ' + shortSha(commit) + '.');
  } catch (err) {
    state.pending.delete(key);
    if (repoKey === state.repoKey) { renderCommitActions(); renderGraph(); }
    toast(err.message, true);
  }
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

async function loadReport() {
  const repo = state.repos.get(state.repoKey);
  const commit = state.commit, variant = state.variant;
  if (!repo || !commit) return;
  clearReport(repo);
  const reportID = ++state.reportID;
  const run = cachedRun(commit, variant);
  if (!run) return;
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
  renderRepos();
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

  for (const alert of alerts) {
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
    tags.appendChild(chip(alert.kind));
    if (alert.status) tags.appendChild(chip(alert.status.toLowerCase(), statusClass(alert.status)));
    head.appendChild(tags);

    head.addEventListener('click', () => {
      if (state.expanded.has(alert.id)) state.expanded.delete(alert.id);
      else state.expanded.add(alert.id);
      renderAlerts();
    });
    item.appendChild(head);

    if (state.expanded.has(alert.id)) {
      item.appendChild(alertBody(alert));
    }
    list.appendChild(item);
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
function alertBody(alert) {
  const body = document.createElement('div');
  body.className = 'alert-body';

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
  return body;
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
    if (event.type === 'repo' && event.repo) {
      state.repos.set(event.repo.key, event.repo);
      rememberRun(event.repo.key, event.repo.latest);
      renderRepos();
      if (event.repo.latest) {
        const run = event.repo.latest;
        const key = pendingKey(event.repo.key, run.commit, run.variant || 'normal');
        if (['queued', 'running'].includes(run.status)) state.pending.set(key, run);
        else state.pending.delete(key);
      }
      if (event.repo.key === state.repoKey) { renderGraph(); renderCommitActions(); }
    } else if (event.type === 'run') {
      const run = event.run;
      rememberRun(event.repo_key, run);
      renderRepos();
      const key = pendingKey(event.repo_key, run.commit, run.variant || 'normal');
      if (['queued', 'running'].includes(run.status)) state.pending.set(key, run);
      else state.pending.delete(key);
      if (event.repo_key === state.repoKey) { renderGraph(); renderCommitActions(); }
    } else if (event.type === 'report') {
      state.pending.delete(pendingKey(event.repo_key, event.commit, event.run.variant || 'normal'));
      rememberRun(event.repo_key, Object.assign({ commit: event.commit }, event.run));
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
  stream.onerror = () => { /* EventSource retries on its own. */ };
}

/* ------------------------------------------------------------------ boot -- */

async function loadRepos() {
  const payload = await api('/api/repos');
  state.repos = new Map((payload.repos || []).map((repo) => [repo.key, repo]));
  state.recent = new Map();
  for (const repo of state.repos.values()) {
    for (const run of repo.recent || []) rememberRun(repo.key, run);
    rememberRun(repo.key, repo.latest);
  }
  renderRepos();
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
  el('mode-label').textContent = 'Hub ' + (me.version || '') + ' · ' + (me.mode || 'lint') + ' mode';

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
  el('only-monitored').addEventListener('change', (event) => {
    state.onlyMonitored = event.target.checked;
    renderRepos();
  });
  el('only-nopolicy').addEventListener('change', (event) => {
    state.onlyMissing = event.target.checked;
    renderRepos();
  });
  renderPeriod();
  el('period').addEventListener('input', (event) => setPeriod(Number(event.target.value)));
  renderSeverity();
  el('severity').addEventListener('input', (event) => setMinSeverity(Number(event.target.value)));
  initSplitter();
  el('refresh-commits').addEventListener('click', () => {
    state.graphs.delete(state.repoKey);
    loadHistory();
  });
  el('modal-close').addEventListener('click', closeModal);
  el('modal').addEventListener('click', (event) => {
    if (event.target === el('modal')) closeModal();
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') closeModal();
  });
  window.addEventListener('hashchange', () => {
    const route = readHash();
    if (route && (route.repoKey !== state.repoKey || route.commit !== state.commit)) {
      selectRepo(route.repoKey, route.commit);
    }
  });

  await loadRepos();
  connectEvents();
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
const SPLIT_KEY = 'swiftproof.commitColumnWidth';
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

document.addEventListener('DOMContentLoaded', boot);
