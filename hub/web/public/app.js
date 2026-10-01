// Probe Hub dashboard.
//
// The page holds three things: the repository list with its bootstrap actions,
// the commit graph and cached plan/normal results, and a live event stream
// that refreshes results without changing the selected commit.
'use strict';

const LEVELS = ['low', 'medium', 'high', 'critical'];
// "Filtered" shows every kind at or above the review threshold; "Everything"
// ignores the threshold. The kind tabs apply it.
const KINDS = [
  { key: 'all', label: 'Filtered' },
  { key: 'everything', label: 'Everything' },
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
  // branch limits the commit tree to one branch of the selected repository;
  // null shows them all.
  branch: null,
  commit: null,
  view: null,
  run: null,
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
  // pendingKey of the selected commit when its stored report could not be
  // loaded, so "Run analysis" stays offered to replace it.
  unreadable: null,
  // Team feedback on the findings of the open report: { commit, learning, entries }.
  feedback: null,
  replyTo: null,
  minSeverity: loadMinSeverity(),
  period: loadPeriod(),
  // Recent normal runs per repository key, keyed by commit, kept up to date
  // by the live events.
  recent: new Map(),
  recentLimit: null,
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

// reviewMark tells who marked the analysis of a commit as reviewed, and when;
// null while nobody has.
function reviewMark(repoKey, commit) {
  return state.repos.get(repoKey)?.reviewed?.[commit] || null;
}

// needsReview is true for a finished run the current threshold flags and
// nobody has marked reviewed yet.
function needsReview(run, repoKey) {
  const summary = run && run.status === 'done' && run.summary;
  return Boolean(summary && summary.verdict === 'review' && !belowThreshold(summary) && !reviewMark(repoKey, run.commit));
}

// reviewedChip replaces "Human review required" once a person reviewed the
// commit; the CLI's request stays in its title.
function reviewedChip(mark, summary) {
  const done = chip('reviewed', 'ok');
  done.title = 'Human review was required at ' + reviewLevel(summary) + ' level; reviewed by ' + mark.by + ' ' + timeAgo(mark.at) + '.';
  return done;
}

function belowThresholdChip(summary) {
  const quiet = chip('review below ' + LEVELS[state.minSeverity], 'below-threshold');
  quiet.title = 'Human review was requested at ' + reviewLevel(summary) + ' level, under the selected threshold.';
  return quiet;
}

function verdictChip(run, repoKey) {
  if (!run) { const unknown = chip('?', 'unknown'); unknown.title = 'No cached result'; return unknown; }
  if (run.status === 'queued') return chip('queued', 'busy');
  if (run.status === 'running') return chip('analyzing…', 'busy');
  if (run.status === 'failed') return chip('analysis failed', 'bad');
  if (run.status === 'cancelled') return chip('cancelled');
  const summary = run.summary || {};
  switch (summary.verdict) {
    case 'blocked': return chip('reproduced issue', 'bad');
    case 'review': {
      const mark = reviewMark(repoKey, run.commit);
      if (mark) return reviewedChip(mark, summary);
      if (belowThreshold(summary)) return belowThresholdChip(summary);
      return chip('Human review required', 'warn ' + reviewTone(summary));
    }
    case 'clear': return chip(run.variant === 'plan' ? 'No plan category flagged' : 'no blocker', 'ok');
    default: return chip(summary.verdict || 'unknown');
  }
}

/* ------------------------------------------------------- repository list -- */

// statusRank orders run statuses by gravity so the worst of a period wins:
// a reproduced issue, then flagged reviews by level, a failed analysis,
// reviews under the threshold, pending analyses, reviewed commits and finally
// clear results.
function statusRank(run, repoKey) {
  if (run.status === 'queued' || run.status === 'running') return 1;
  if (run.status === 'failed') return 20;
  const summary = run.summary || {};
  switch (summary.verdict) {
    case 'blocked': return 40;
    case 'review': {
      if (reviewMark(repoKey, run.commit)) return 0.5;
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
    if (!worst || statusRank(run, repo.key) > statusRank(worst, repo.key) ||
        (statusRank(run, repo.key) === statusRank(worst, repo.key) && runActivity(run) > runActivity(worst))) {
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
      meta.appendChild(verdictChip(worst, repo.key));
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
  const repos = Array.from(state.repos.values()).filter((repo) => needsReview(worstRun(repo), repo.key)).length;
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
  const rules = button('', 'btn quiet small icon-btn' + (repo.coding_rules ? ' has-rules' : ''), (event) => {
    event.stopPropagation();
    openRulesDialog(repo);
  });
  rules.appendChild(gearIcon());
  rules.setAttribute('aria-label', repo.coding_rules ? 'Review settings (coding rules set)' : 'Review settings');
  rules.title = 'Review settings: monitoring, coding rules and learning from team feedback'
    + (repo.coding_rules ? ' (coding rules set)' : '');
  if (!repo.has_policy) {
    const addPolicy = button('Add policy', 'btn setup small', (event) => {
      event.stopPropagation();
      openPolicyDialog(repo);
    });
    addPolicy.title = 'Create a .probe.json policy';
    actions.appendChild(addPolicy);
    actions.appendChild(rules);
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
  actions.appendChild(rules);
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

// gearIcon draws the cog of the review settings button.
function gearIcon() {
  const svg = svgElement('svg', { width: 14, height: 14, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' });
  svg.appendChild(svgElement('circle', { cx: 12, cy: 12, r: 3 }));
  svg.appendChild(svgElement('path', { d: 'M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z' }));
  return svg;
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
    + '. Monitoring can be activated separately after adding the policy. Review the sandbox image and the commands before relying on a report; '
    + 'you can edit the policy below before committing it.';
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

  // The generated policy, editable before it is committed.
  const preview = document.createElement('textarea');
  preview.id = 'policy-text';
  preview.className = 'policy-text';
  preview.rows = 18;
  preview.spellcheck = false;
  preview.maxLength = 65536;
  preview.setAttribute('aria-label', 'Policy (.probe.json)');
  preview.value = 'Generating a preview…';
  preview.disabled = true;
  body.appendChild(preview);
  const problem = document.createElement('p');
  problem.id = 'policy-problem';
  problem.className = 'note bad';
  problem.setAttribute('aria-live', 'polite');
  body.appendChild(problem);
  let generated = '';
  // checkPolicy refuses what is not a JSON object; the CLI checks the fields.
  const checkPolicy = () => {
    let value;
    try { value = JSON.parse(preview.value); } catch (err) { value = undefined; problem.textContent = 'Invalid JSON: ' + err.message; }
    if (value !== undefined) problem.textContent = value && typeof value === 'object' && !Array.isArray(value) ? '' : 'The policy must be a JSON object.';
    create.disabled = problem.textContent !== '';
  };
  preview.addEventListener('input', checkPolicy);

  const create = button('Commit the policy', 'btn', async () => {
    create.disabled = true;
    try {
      const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/policy', {
        method: 'POST',
        body: { language: select.value === 'auto' ? '' : select.value, policy: preview.value === generated ? '' : preview.value },
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
    if (generated && preview.value !== generated && !window.confirm('Replace your edits with the policy generated for ' + select.value + '?')) {
      select.value = select.dataset.loaded;
      return;
    }
    select.dataset.loaded = select.value;
    generated = '';
    preview.value = 'Generating a preview…';
    preview.disabled = true;
    problem.textContent = '';
    create.disabled = true;
    try {
      const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/policy', {
        method: 'POST',
        body: { language: select.value === 'auto' ? '' : select.value, preview: true },
      });
      generated = payload.policy;
      preview.value = payload.policy;
      preview.disabled = false;
      if (select.value === 'auto') {
        intro.textContent = 'Detected language: ' + payload.language + '. ' + intro.textContent;
      }
      checkPolicy();
    } catch (err) {
      preview.value = err.message;
    }
  };
  select.addEventListener('change', loadPreview);
  el('modal').classList.remove('hidden');
  loadPreview();
}

// openRulesDialog edits the team coding rules of a repository. The hub gives
// them to the AI reviewer of every later review, which checks the changed code
// against them; an empty text removes them.
function openRulesDialog(repo) {
  closeModal();
  const modalBody = el('modal-body');
  const footer = el('modal-footer');
  el('modal-title').textContent = 'Review settings · ' + repo.full_name;
  modalBody.textContent = '';
  footer.textContent = '';
  // Two tabs: the settings saved with the footer's button, and the history
  // of the commits people marked reviewed.
  const body = document.createElement('div');
  body.id = 'review-settings-panel';
  body.setAttribute('role', 'tabpanel');
  const history = document.createElement('div');
  history.id = 'review-history-panel';
  history.setAttribute('role', 'tabpanel');
  history.hidden = true;
  const tabs = document.createElement('div');
  tabs.className = 'tabs';
  tabs.setAttribute('role', 'tablist');
  const showTab = (panel) => {
    for (const tab of tabs.children) tab.setAttribute('aria-selected', String(tab.panel === panel));
    body.hidden = panel !== body;
    history.hidden = panel !== history;
    footer.hidden = panel !== body;
    if (panel === history) renderReviewHistory(repo, history);
  };
  for (const [label, panel, id] of [['Coding rules', body, 'tab-coding-rules'], ['Review history', history, 'tab-review-history']]) {
    const tab = button(label, 'tab', () => showTab(panel));
    tab.id = id;
    tab.panel = panel;
    tab.setAttribute('role', 'tab');
    tab.setAttribute('aria-selected', String(panel === body));
    panel.setAttribute('aria-labelledby', id);
    tabs.appendChild(tab);
  }
  modalBody.append(tabs, body, history);

  const intro = document.createElement('p');
  intro.className = 'note';
  intro.textContent = 'The AI reviewer checks the changed code of every review against these rules and reports each violation it finds as an alert. '
    + 'They apply only when an AI reviewer runs, never to lint-only analyses. One rule per line works best.';
  body.appendChild(intro);

  const text = document.createElement('textarea');
  text.id = 'coding-rules';
  text.className = 'search rules-text';
  text.rows = 12;
  text.maxLength = 32768;
  text.placeholder = '- Never log credentials or tokens.\n- Wrap returned errors with context.\n- Every public function has a test.';
  text.value = repo.coding_rules || '';
  text.setAttribute('aria-label', 'Coding rules');
  const rulesTitle = document.createElement('h3');
  rulesTitle.className = 'settings-title';
  rulesTitle.textContent = 'Coding rules';
  body.insertBefore(rulesTitle, intro);
  body.appendChild(text);

  // Learning from team feedback: on unless the owner switched it off.
  const learningTitle = document.createElement('h3');
  learningTitle.className = 'settings-title';
  learningTitle.textContent = 'Learning from team feedback';
  body.appendChild(learningTitle);
  const learningNote = document.createElement('p');
  learningNote.className = 'note';
  learningNote.textContent = 'Votes, comments and replies on findings, and whether the next analyzed commit changed the file a finding was about, '
    + 'adapt how the AI reviewer investigates and words later reviews. They never change a verdict by themselves.';
  body.appendChild(learningNote);
  const toggle = document.createElement('label');
  toggle.className = 'row';
  const enabled = document.createElement('input');
  enabled.type = 'checkbox';
  enabled.id = 'learning-enabled';
  enabled.checked = repo.learning !== false;
  toggle.appendChild(enabled);
  toggle.appendChild(document.createTextNode(' Learn from team feedback'));
  body.appendChild(toggle);
  const learned = document.createElement('div');
  learned.id = 'learned-summary';
  learned.className = 'note';
  learned.textContent = 'Loading what was learned…';
  body.appendChild(learned);
  const showLearned = (feedback) => {
    learned.textContent = '';
    const topics = feedback?.topics || [];
    if (topics.length === 0 && (feedback?.comments || []).length === 0) {
      learned.textContent = 'Nothing learned yet: vote or comment on findings in a report.';
      return;
    }
    const list = document.createElement('ul');
    for (const t of topics) {
      const item = document.createElement('li');
      item.textContent = t.topic + ': useful ' + t.useful + ', not useful ' + t.not_useful
        + '; code changed after it ' + t.changed + ', left unchanged ' + t.unchanged;
      list.appendChild(item);
    }
    learned.appendChild(list);
    const comments = document.createElement('div');
    comments.textContent = (feedback.comments || []).length + ' recent comments are quoted to the reviewer.';
    learned.appendChild(comments);
  };
  api('/api/repos/' + encodeURIComponent(repo.key) + '/learning')
    .then((payload) => showLearned(payload.feedback))
    .catch((err) => { learned.textContent = err.message; });
  const reset = button('Forget feedback', 'btn quiet small', async () => {
    if (!window.confirm('Forget every vote, comment and outcome learned for ' + repo.full_name + '?')) return;
    try {
      const payload = await api('/api/repos/' + encodeURIComponent(repo.key) + '/feedback', { method: 'DELETE' });
      upsertRepo(payload.repo);
      showLearned(payload.feedback);
      toast('Feedback forgotten.');
    } catch (err) {
      toast(err.message, true);
    }
  });
  reset.id = 'learning-reset';
  body.appendChild(reset);

  // Monitoring: stopping removes the webhook, so later commits are no longer analyzed.
  if (repo.monitored) {
    const monitoringTitle = document.createElement('h3');
    monitoringTitle.className = 'settings-title';
    monitoringTitle.textContent = 'Monitoring';
    body.appendChild(monitoringTitle);
    const monitoringNote = document.createElement('p');
    monitoringNote.className = 'note';
    monitoringNote.textContent = 'Every new commit of ' + repo.full_name + ' is analyzed. Stopping removes the webhook; existing results are kept.';
    body.appendChild(monitoringNote);
    const stop = button('Stop monitoring', 'btn quiet small', async () => {
      stop.disabled = true;
      closeModal();
      await setMonitoring(repo, false);
    });
    stop.id = 'stop-monitoring';
    body.appendChild(stop);
  }

  const save = button('Save', 'btn', async () => {
    save.disabled = true;
    try {
      const saved = await api('/api/repos/' + encodeURIComponent(repo.key) + '/rules', {
        method: 'PUT',
        body: { rules: text.value },
      });
      upsertRepo(saved.repo);
      if (enabled.checked !== (repo.learning !== false)) {
        const switched = await api('/api/repos/' + encodeURIComponent(repo.key) + '/learning', {
          method: 'PUT',
          body: { enabled: enabled.checked },
        });
        upsertRepo(switched.repo);
      }
      closeModal();
      toast('Review settings saved.');
    } catch (err) {
      toast(err.message, true);
      save.disabled = false;
    }
  });
  save.id = 'coding-rules-save';
  footer.appendChild(button('Cancel', 'btn quiet', closeModal));
  footer.appendChild(save);
  el('modal').classList.remove('hidden');
  text.focus();
}

// renderReviewHistory lists, newest first, who marked which commit of a
// repository reviewed or withdrew the mark. A commit opens its report.
async function renderReviewHistory(repo, panel) {
  panel.textContent = '';
  const status = document.createElement('p');
  status.className = 'note';
  status.textContent = 'Loading the review history…';
  panel.appendChild(status);
  let reviews;
  try {
    reviews = (await api('/api/repos/' + encodeURIComponent(repo.key) + '/reviews')).reviews || [];
  } catch (err) {
    status.textContent = err.message;
    return;
  }
  if (!reviews.length) {
    status.textContent = 'No review recorded yet: “Mark as reviewed”, at the top of a report that asks for a human review, records one.';
    return;
  }
  status.textContent = 'The latest ' + reviews.length + ' review actions, newest first. Marking a commit reviewed never changes its analysis.';
  const list = document.createElement('ul');
  list.className = 'review-history';
  for (const entry of reviews) {
    const item = document.createElement('li');
    item.appendChild(entry.reviewed ? chip('reviewed', 'ok') : chip('mark withdrawn'));
    const open = button(shortSha(entry.commit), 'ref-link mono', () => { closeModal(); selectRepo(repo.key, entry.commit); });
    open.title = 'Open the report of ' + entry.commit;
    item.appendChild(open);
    const message = document.createElement('span');
    message.className = 'review-message';
    message.textContent = entry.message || '';
    item.appendChild(message);
    const who = document.createElement('span');
    who.className = 'note';
    who.textContent = entry.by + ' · ' + timeAgo(entry.at);
    who.title = new Date(entry.at).toLocaleString();
    item.appendChild(who);
    list.appendChild(item);
  }
  panel.appendChild(list);
}

/* ----------------------------------------------------- PR summary -- */

// The pull request summary the reviewer model wrote after the review is the
// report itself: this head carries its title, overview and the statements
// that span the change, and the alert list below is laid out by its change
// areas (renderAlerts). Every statement ends with links to the code it cites,
// which unfold the diff under it, and a risk with the alerts it cites. The
// text is rendered as text only; what Probe computed (alerts, checks, the
// verdict) never comes from the model. A button copies it as Markdown for a
// pull request description.
function renderPRSummary(s) {
  const box = document.createElement('section');
  box.className = 'reviewer-summary pr-summary';
  box.setAttribute('aria-label', 'AI report');
  const head = document.createElement('div');
  head.className = 'pr-summary-title';
  const title = document.createElement('b');
  title.textContent = s.title || '';
  head.append(title, chip('AI report', 'busy'));
  box.appendChild(head);
  const overview = document.createElement('p');
  overview.textContent = s.overview || '';
  box.appendChild(overview);
  const section = (title, items, fill) => {
    if (!items || !items.length) return null;
    if (title) {
      const h = document.createElement('b');
      h.textContent = title;
      box.appendChild(h);
    }
    const list = document.createElement('ul');
    for (const item of items) {
      const li = document.createElement('li');
      fill(li, item);
      list.appendChild(li);
    }
    box.appendChild(list);
    return list;
  };
  const point = (li, p) => {
    const text = document.createElement('span');
    text.textContent = p.text;
    li.append(text, summaryCitations(li, p.refs, []));
  };
  section('Behavior changes', s.behavior_changes, point);
  // Risks and review focus points carry a severity and follow the filters.
  for (const [key, title] of [['risks', 'Risks'], ['focus', 'Where to look first']]) {
    if (!(RATED[key].items(s) || []).length) continue;
    const h = document.createElement('b');
    h.textContent = title;
    const holder = document.createElement('div');
    holder.className = 'pr-rated';
    holder.id = 'pr-' + key;
    holder.dataset.section = key;
    box.append(h, holder);
    renderRated(holder, s);
  }
  const h = document.createElement('b');
  h.textContent = 'Testing';
  box.appendChild(h);
  section(null, s.testing, point);
  const executed = document.createElement('p');
  executed.className = 'note';
  executed.textContent = checksSentence((state.view && state.view.checks) || []);
  box.appendChild(executed);

  const footer = document.createElement('div');
  footer.className = 'row';
  const caveat = document.createElement('span');
  caveat.className = 'note';
  caveat.textContent = 'Written by ' + (s.model || 'the reviewer model') + ' after the review, which groups the alerts below by change area. Model output, not evidence: alerts, checks and the verdict come from Probe.'
    + (summaryQuoted(s) ? ' ✓ marks a citation whose quoted code Probe found at those lines; what the summary says about it remains model output.' : '')
    + (s.rejected_citations ? ' ' + s.rejected_citations + (s.rejected_citations === 1 ? ' citation quoted code that is not in the diff and was dropped.' : ' citations quoted code that is not in the diff and were dropped.') : '');
  const copy = button('Copy as Markdown', 'btn quiet small', async () => {
    try {
      await navigator.clipboard.writeText(prSummaryMarkdown(s));
      toast('Summary copied.');
    } catch (err) {
      toast('Could not copy: ' + err.message, true);
    }
  });
  footer.append(caveat, copy);
  box.appendChild(footer);
  return box;
}

// riskAlerts returns the alerts a risk cites, once each.
function riskAlerts(risk) {
  const ids = (risk.hypothesis_ids || []).map((id) => 'issue:' + id).concat((risk.signal_ids || []).map((id) => 'signal:' + id));
  return [...new Set(ids.map(findAlert).filter(Boolean))];
}

// riskShown applies the alert filters to a risk: its severity is the
// model's estimate, so it obeys the review threshold like the alerts, except
// on Everything; a risk citing an alert the threshold shows stays shown.
function riskShown(risk) {
  if (state.kind === 'everything') return true;
  return LEVELS.indexOf(risk.severity) >= state.minSeverity || riskAlerts(risk).some(aboveThreshold);
}

// focusShown applies the alert filters to a review focus point, whose
// severity the CLI raised to the findings on the lines it cites.
function focusShown(focus) {
  return state.kind === 'everything' || LEVELS.indexOf(focus.severity) >= state.minSeverity;
}

// RATED describes the sections of the summary head whose statements carry a
// severity: the model's estimate, never below the findings they cite.
const RATED = {
  risks: {
    items: (s) => s.risks,
    shown: riskShown,
    alerts: (risk) => riskAlerts(risk).map((a) => a.id),
    nouns: ['risk is', 'risks are'],
    title: 'Severity estimated by the AI reviewer, at least that of the findings it cites. Model output, not evidence.',
  },
  focus: {
    items: (s) => s.review_focus,
    shown: focusShown,
    alerts: () => [],
    nouns: ['place to look is', 'places to look are'],
    title: 'Severity estimated by the AI reviewer, at least that of the findings on the cited lines. Model output, not evidence.',
  },
};

function ratedFilterKey() {
  return state.kind + ':' + state.minSeverity + ':' + ((state.view && state.view.alerts) || []).length;
}

// renderRated fills a rated section of the summary head, most severe first as
// the CLI ordered it, each statement led by its severity in its color. The
// filters hide what is below the review threshold, as for the alerts; it runs
// again whenever they change.
function renderRated(holder, s) {
  const rated = RATED[holder.dataset.section];
  holder.textContent = '';
  holder.dataset.filter = ratedFilterKey();
  const list = document.createElement('ul');
  let hidden = 0;
  for (const item of rated.items(s) || []) {
    if (!rated.shown(item)) {
      hidden++;
      continue;
    }
    const li = document.createElement('li');
    li.className = 'rated ' + severityClass(item.severity);
    const text = document.createElement('span');
    text.textContent = item.text;
    if (item.severity) {
      const level = dotChip(item.severity, item.severity);
      level.title = rated.title;
      li.append(level);
    }
    li.append(text, summaryCitations(li, item.refs, rated.alerts(item)));
    list.appendChild(li);
  }
  if (list.children.length) holder.appendChild(list);
  if (hidden) {
    const note = document.createElement('p');
    note.className = 'note';
    note.textContent = hidden + ' ' + rated.nouns[hidden === 1 ? 0 : 1] + ' below the selected severity. Lower the filter or choose Everything to see ' + (hidden === 1 ? 'it.' : 'them.');
    holder.appendChild(note);
  }
}

// areaItem heads the alerts of one change area of the summary: its purpose,
// what changed, the code it cites, and how many of its alerts the filters
// show.
function areaItem(change, shown, total) {
  const item = document.createElement('li');
  item.className = 'area';
  const head = document.createElement('div');
  head.className = 'area-head';
  const name = document.createElement('b');
  name.textContent = change.area;
  const count = document.createElement('span');
  count.className = 'note';
  count.textContent = total === 0 ? 'no alert' : (shown === total ? '' : shown + ' of ') + total + (total === 1 ? ' alert' : ' alerts');
  head.append(name, count);
  const summary = document.createElement('p');
  summary.textContent = change.summary;
  summary.appendChild(summaryCitations(item, change.refs, []));
  item.append(head, summary);
  return item;
}

function otherAreaItem(shown) {
  const item = document.createElement('li');
  item.className = 'area other';
  const head = document.createElement('div');
  head.className = 'area-head';
  const name = document.createElement('b');
  name.textContent = 'Other alerts';
  const count = document.createElement('span');
  count.className = 'note';
  count.textContent = shown + (shown === 1 ? ' alert' : ' alerts') + ' the summary does not cite';
  head.append(name, count);
  item.appendChild(head);
  return item;
}

// summaryCitations renders the links of one summary statement: the code it
// cites, then the alerts (by alert ID prefix) a risk cites. A code link
// unfolds its diff under the statement held by li; a second click folds it.
// An alert the list shows is only named: jumping down to the list lost the
// reader's place. Any other alert unfolds whole under the statement, which
// is then its only place in the report.
function summaryCitations(li, refs, alertIDs) {
  const links = document.createElement('span');
  links.className = 'summary-refs';
  let open = null;
  const toggle = (link, fill) => {
    const panel = li.querySelector(':scope > .summary-diff');
    if (panel) panel.remove();
    if (open) open.setAttribute('aria-expanded', 'false');
    if (open === link) {
      open = null;
      return;
    }
    open = link;
    link.setAttribute('aria-expanded', 'true');
    const holder = document.createElement('div');
    holder.className = 'summary-diff';
    fill(holder);
    li.appendChild(holder);
  };
  for (const ref of refs || []) {
    const link = refLink(refLabel(ref), ref.quote ? ref.path + '\nQuoted code found at these lines:\n' + ref.quote : ref.path);
    if (ref.quote) link.classList.add('verified');
    const target = { path: ref.path, line: ref.start_line || 0, end_line: ref.end_line || ref.start_line || 0, side: ref.side === 'old' ? 'old' : 'new' };
    link.addEventListener('click', () => toggle(link, (holder) => appendAlertDiff(holder, target)));
    links.appendChild(link);
  }
  for (const alert of new Set(alertIDs.map(findAlert).filter(Boolean))) {
    if (inAlertList(alert)) {
      const mention = document.createElement('span');
      mention.className = 'ref-link alert-ref mention';
      mention.textContent = alert.title || alert.id;
      mention.title = alertLocation(alert);
      links.appendChild(mention);
      continue;
    }
    const link = refLink(alert.title || alert.id, alertLocation(alert));
    link.classList.add('alert-ref');
    link.addEventListener('click', () => toggle(link, (holder) => holder.appendChild(alertBody(alert))));
    links.appendChild(link);
  }
  return links;
}

function refLink(text, title) {
  const link = document.createElement('button');
  link.type = 'button';
  link.className = 'ref-link mono';
  link.textContent = text;
  link.title = title;
  link.setAttribute('aria-expanded', 'false');
  return link;
}

// refLabel names a code reference by file name and lines; the full path is in
// the link's title.
function refLabel(ref) {
  let label = ref.path.split('/').pop();
  if (ref.start_line) {
    label += ':' + ref.start_line;
    if (ref.end_line && ref.end_line > ref.start_line) label += '-' + ref.end_line;
    if (ref.side === 'old') label += ' (old)';
  }
  return ref.quote ? '✓ ' + label : label;
}

function refText(ref) {
  let text = ref.path;
  if (ref.start_line) {
    text += ':' + ref.start_line;
    if (ref.end_line && ref.end_line > ref.start_line) text += '-' + ref.end_line;
    if (ref.side === 'old') text += ' (old)';
  }
  return ref.quote ? text + ' ✓' : text;
}

// summaryQuoted reports whether a summary cites any verified quote.
function summaryQuoted(s) {
  const lists = [(s.changes || []), (s.behavior_changes || []), (s.risks || []), (s.review_focus || []), (s.testing || [])];
  return lists.some((items) => items.some((item) => (item.refs || []).some((ref) => ref.quote)));
}

// findAlert finds the alert built from a signal or hypothesis ("signal:<id>",
// "issue:<id>"), split by line ("#n") or grouped with other signals.
function findAlert(id) {
  const view = state.view;
  if (!view) return null;
  const matches = (a) => a.id === id || a.id.startsWith(id + '#');
  for (const alert of (view.alerts || []).concat(view.dismissed || [])) {
    if (matches(alert) || (alert.members || []).some(matches)) return alert;
  }
  return null;
}

// inAlertList reports whether renderAlerts lists an alert: one the filters
// keep, under its change area or, when no risk cites it, under the others.
// The summary is drawn before the list, so this follows its rules rather
// than looking for the item.
function inAlertList(alert) {
  if (!filteredAlerts().includes(alert)) return false;
  const summary = state.view.pr_summary;
  const areas = (summary && summary.changes) || [];
  if (areas.length === 0 || (alert.area && alert.area <= areas.length)) return true;
  return !riskCitedAlerts(summary).has(alert.id);
}

// riskCitedAlerts returns the IDs of the alerts the summary's risks cite.
function riskCitedAlerts(s) {
  const ids = new Set();
  for (const risk of (s && s.risks) || []) {
    for (const id of (risk.hypothesis_ids || []).map((h) => 'issue:' + h).concat((risk.signal_ids || []).map((g) => 'signal:' + g))) {
      const alert = findAlert(id);
      if (alert) ids.add(alert.id);
    }
  }
  return ids;
}

// checksSentence states, from the report, what Probe executed: the testing
// part of the summary never comes from the model.
function checksSentence(checks) {
  if (!checks.length) return 'Probe executed no check for this review.';
  const counts = new Map();
  for (const c of checks) counts.set(c.status, (counts.get(c.status) || 0) + 1);
  const parts = [...counts].map(([status, n]) => n + ' ' + status);
  return 'Probe executed ' + checks.length + (checks.length === 1 ? ' check' : ' checks') + ' for this review: ' + parts.join(', ') + '.';
}

function prSummaryMarkdown(s) {
  const lines = ['# ' + (s.title || ''), '', s.overview || ''];
  const cites = (refs) => (refs && refs.length ? ' — ' + refs.map(refText).join(', ') : '');
  const list = (title, items) => {
    if (!items || !items.length) return;
    lines.push('', '## ' + title, '');
    for (const item of items) lines.push('- ' + item);
  };
  const points = (items) => (items || []).map((p) => p.text + cites(p.refs));
  // Each area lists the alerts it groups, once each, as Probe recorded them.
  list('Changes', (s.changes || []).map((c) => {
    const ids = (c.hypothesis_ids || []).map((id) => 'issue:' + id).concat((c.signal_ids || []).map((id) => 'signal:' + id));
    const alerts = [...new Set(ids.map(findAlert).filter(Boolean))];
    return ['**' + c.area + '**: ' + c.summary + cites(c.refs)]
      .concat(alerts.map((a) => '  - ' + (a.status ? '**' + a.status + ' / ' + a.severity + '** ' : '') + (a.title || a.id) + ' — ' + alertLocation(a)))
      .join('\n');
  }));
  list('Behavior changes', points(s.behavior_changes));
  list('Risks', (s.risks || []).map((r) => {
    const alerts = (r.hypothesis_ids || []).map((id) => findAlert('issue:' + id)).concat((r.signal_ids || []).map((id) => findAlert('signal:' + id))).filter(Boolean);
    const where = (r.refs || []).map(refText).concat([...new Set(alerts)].map((a) => (a.title || a.id) + ' — ' + alertLocation(a) + ' (' + (a.status ? a.status.toLowerCase() + ' ' : '') + a.kind + ')'));
    return (r.severity ? '**' + r.severity + '** ' : '') + r.text + (where.length ? ' — ' + where.join(', ') : '');
  }));
  list('Where to look first', (s.review_focus || []).map((f) => (f.severity ? '**' + f.severity + '** ' : '') + f.text + cites(f.refs)));
  lines.push('', '## Testing', '');
  for (const item of points(s.testing)) lines.push('- ' + item);
  if (s.testing && s.testing.length) lines.push('');
  lines.push('_' + checksSentence((state.view && state.view.checks) || []) + '_');
  lines.push('', '---', '', '_Written by ' + (s.model || 'the reviewer model') + ' from the Probe review. Model output, not evidence._');
  return lines.join('\n') + '\n';
}

/* ------------------------------------------------------- agent access -- */

// openLanguageDialog chooses the account-wide language the AI reviewer writes
// reports in. Probe's own labels, verdicts and checks stay in English.
function openLanguageDialog() {
  closeModal();
  const body = el('modal-body');
  const footer = el('modal-footer');
  el('modal-title').textContent = 'Language';
  body.textContent = '';
  footer.textContent = '';
  const intro = document.createElement('p');
  intro.className = 'note';
  intro.textContent = 'The AI reviewer writes its findings, the AI report and plans in this language, for every repository of this account. '
    + 'It applies to analyses queued from now on: earlier reports keep their language until rerun. Probe\'s own labels, verdicts and check results stay in English.';
  body.appendChild(intro);
  const label = document.createElement('label');
  label.className = 'row';
  label.textContent = 'Report language ';
  const select = document.createElement('select');
  select.id = 'report-language';
  for (const language of state.me?.report_languages || ['English']) {
    const option = document.createElement('option');
    option.value = language;
    option.textContent = language;
    select.appendChild(option);
  }
  select.value = state.me?.settings?.report_language || 'English';
  label.appendChild(select);
  body.appendChild(label);
  const save = button('Save', 'btn', async () => {
    save.disabled = true;
    try {
      const saved = await api('/api/settings', { method: 'PUT', body: { report_language: select.value } });
      state.me.settings = saved.settings;
      closeModal();
      toast('Reports will be written in ' + saved.settings.report_language + '.');
    } catch (err) {
      toast(err.message, true);
      save.disabled = false;
    }
  });
  save.id = 'report-language-save';
  footer.appendChild(button('Cancel', 'btn quiet', closeModal));
  footer.appendChild(save);
  el('modal').classList.remove('hidden');
  select.focus();
}

// Agent tokens let coding agents (Claude Code, Cursor, …) use the hub's MCP
// endpoint. A token is shown once, right after it is created.
async function openAgentDialog() {
  closeModal();
  el('modal-title').textContent = 'Agent access (MCP)';
  const body = el('modal-body');
  body.textContent = '';
  const intro = document.createElement('p');
  intro.className = 'note';
  intro.textContent = 'Coding agents connect to this hub over the Model Context Protocol with a token. ' +
    'A read token lists repositories and reads findings; a write token also triggers, reruns and cancels analyses and changes coding rules and learning. ' +
    'When this hub lends its LLM, an LLM gateway token lets the Probe CLI use it, for example in CI; on a workstation, probe login creates one for you.';
  const usage = document.createElement('p');
  usage.className = 'note hidden';
  usage.id = 'agent-llm-usage';
  const created = document.createElement('div');
  created.id = 'agent-token-created';
  const list = document.createElement('div');
  list.id = 'agent-token-list';
  list.textContent = 'Loading tokens…';

  const form = document.createElement('div');
  form.className = 'row';
  const name = document.createElement('input');
  name.className = 'search';
  name.id = 'agent-token-name';
  name.placeholder = 'Token name, e.g. Claude Code on my laptop';
  name.maxLength = 80;
  const scope = document.createElement('select');
  scope.id = 'agent-token-scope';
  for (const [value, label] of [['read', 'Read'], ['write', 'Read and write']]) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    scope.appendChild(option);
  }
  const expiry = document.createElement('select');
  expiry.id = 'agent-token-expiry';
  for (const [value, label] of [['30', '30 days'], ['90', '90 days'], ['366', '1 year'], ['0', 'No expiry']]) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    expiry.appendChild(option);
  }
  form.append(name, scope, expiry);
  body.append(intro, usage, form, created, list);

  const create = button('Create token', 'btn small', async () => {
    create.disabled = true;
    try {
      const out = await api('/api/tokens', { method: 'POST', body: { name: name.value, scope: scope.value, expires_days: Number(expiry.value) } });
      name.value = '';
      showCreatedToken(created, out);
      await refreshAgentTokens();
    } catch (err) {
      toast(err.message, true);
    }
    create.disabled = false;
  });
  el('modal-footer').replaceChildren(button('Close', 'btn quiet', closeModal), create);
  el('modal').classList.remove('hidden');
  name.focus();
  await refreshAgentTokens();
}

function showCreatedToken(holder, out) {
  holder.textContent = '';
  const warn = document.createElement('p');
  warn.className = 'notice';
  warn.textContent = 'Copy this token now: it will not be shown again.';
  const token = document.createElement('pre');
  token.className = 'alert-detail mono';
  token.textContent = out.token;
  if (out.info && out.info.scope === 'llm') {
    const ci = document.createElement('p');
    ci.className = 'note';
    ci.textContent = 'Probe CLI, for example as CI variables (keep the token a secret):';
    const env = document.createElement('pre');
    env.className = 'alert-detail mono';
    env.textContent = 'PROBE_HUB_URL=' + window.location.origin + '\nPROBE_HUB_TOKEN=' + out.token;
    holder.append(warn, token, ci, env);
    return;
  }
  const how = document.createElement('p');
  how.className = 'note';
  how.textContent = 'Claude Code:';
  const claude = document.createElement('pre');
  claude.className = 'alert-detail mono';
  claude.textContent = 'claude mcp add --transport http probe-hub ' + out.endpoint + ' --header "Authorization: Bearer ' + out.token + '"';
  const cursorNote = document.createElement('p');
  cursorNote.className = 'note';
  cursorNote.textContent = 'Cursor and other clients (mcp.json):';
  const cursor = document.createElement('pre');
  cursor.className = 'alert-detail mono';
  cursor.textContent = JSON.stringify({ mcpServers: { 'probe-hub': { url: out.endpoint, headers: { Authorization: 'Bearer ' + out.token } } } }, null, 2);
  holder.append(warn, token, how, claude, cursorNote, cursor);
}

async function refreshAgentTokens() {
  const list = el('agent-token-list');
  if (!list) return;
  let data;
  try {
    data = await api('/api/tokens');
  } catch (err) {
    list.textContent = 'Could not load the tokens: ' + err.message;
    return;
  }
  list.textContent = '';
  const usage = el('agent-llm-usage');
  if (usage && data.llm) {
    usage.textContent = 'LLM gateway: ' + data.llm.used_today.toLocaleString() + ' of ' +
      data.llm.daily_tokens.toLocaleString() + ' tokens used today (resets at 00:00 UTC).';
    usage.classList.remove('hidden');
    const scope = el('agent-token-scope');
    if (scope && !scope.querySelector('option[value="llm"]')) {
      const option = document.createElement('option');
      option.value = 'llm';
      option.textContent = 'LLM gateway (CLI)';
      scope.appendChild(option);
    }
  }
  const tokens = data.tokens || [];
  if (!tokens.length) {
    const empty = document.createElement('p');
    empty.className = 'note';
    empty.textContent = 'No agent token yet. Endpoint: ' + data.endpoint;
    list.appendChild(empty);
    return;
  }
  for (const token of tokens) {
    const row = document.createElement('div');
    row.className = 'row activity-row';
    const label = document.createElement('span');
    label.textContent = token.name + ' · ' + ({ write: 'read and write', llm: 'LLM gateway' }[token.scope] || 'read');
    const dates = document.createElement('span');
    dates.className = 'note';
    dates.textContent = 'created ' + new Date(token.created_at).toLocaleDateString() +
      (token.expires_at ? ' · expires ' + new Date(token.expires_at).toLocaleDateString() : ' · no expiry') +
      (token.last_used_at ? ' · last used ' + new Date(token.last_used_at).toLocaleString() : ' · never used');
    const spacer = document.createElement('span');
    spacer.className = 'spacer';
    const revoke = button('Revoke', 'btn quiet small', async () => {
      revoke.disabled = true;
      try {
        await api('/api/tokens/' + encodeURIComponent(token.id), { method: 'DELETE' });
        toast('Token revoked.');
        await refreshAgentTokens();
      } catch (err) {
        toast(err.message, true);
        revoke.disabled = false;
      }
    });
    row.append(label, dates, spacer, revoke);
    list.appendChild(row);
  }
}

let activityTimer;
let activityDialogID = 0;
let activityRequestID = 0;
let activityOpen = false;

function closeModal() {
  el('modal').classList.add('hidden');
  el('modal-footer').hidden = false;
  clearInterval(activityTimer);
  activityDialogID++;
  if (activityOpen) el('settings').focus();
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
  if (state.repoKey !== repoKey && state.branch) {
    // The cached tree holds one branch only: the next visit loads them all.
    state.graphs.delete(state.repoKey);
    state.branch = null;
  }
  state.repoKey = repoKey;
  state.commit = commit || null;
  state.expanded.clear();
  const hash = '#/repo/' + repoKey + (commit ? '/commit/' + commit : '');
  if (window.location.hash !== hash) window.location.hash = hash;
  renderRepos();
  clearReport(state.repos.get(repoKey));
  el('commit-tree').textContent = '';
  el('branches').textContent = '';
  el('commit-actions').classList.add('hidden');
  await loadHistory();
}

async function loadHistory() {
  const repo = state.repos.get(state.repoKey);
  if (!repo) return;
  const loadID = ++state.loadID;
  el('commit-browser').classList.remove('hidden');
  el('splitter').classList.remove('hidden');
  el('graph-note').textContent = 'Loading commits…';
  // History remains useful if fetching Git temporarily fails.
  const results = await Promise.allSettled([
    state.graphs.has(repo.key) ? Promise.resolve(state.graphs.get(repo.key))
      : api(commitsPath(repo)),
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
    renderCommitActions();
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

// commitsPath is where the commit tree of a repository is read, limited to
// the selected branch, if any.
function commitsPath(repo) {
  return '/api/repos/' + encodeURIComponent(repo.key) + '/commits'
    + (state.branch ? '?branch=' + encodeURIComponent(state.branch) : '');
}

// selectBranch reloads the tree with one branch only, its whole fetched
// history in the window; selecting it again, or "All branches", shows them
// all. A branch deleted meanwhile falls back to every branch.
async function selectBranch(name) {
  const repo = state.repos.get(state.repoKey);
  if (!repo) return;
  state.branch = state.branch === name ? null : name;
  state.graphs.delete(repo.key);
  await loadHistory();
  if (state.branch && state.repoKey === repo.key && !state.graphs.has(repo.key)) {
    toast('Branch ' + name + ' could not be loaded; showing every branch.', true);
    state.branch = null;
    await loadHistory();
  }
}

function renderGraph() {
  renderReviewCount();
  const graph = state.graphs.get(state.repoKey);
  if (!graph) return;
  const commits = graph.commits || [];
  const scope = state.branch ? 'Branch ' + state.branch + ' only' : 'All branches';
  el('graph-note').textContent = !commits.length ? 'This repository has no commits yet.'
    : graph.limited ? scope + ' · recent history: up to 300 commits, fetched to a depth of 100. Older parents may be outside this view.'
      : scope + ' · select a commit · ? means no cached result.';
  const branches = el('branches');
  branches.textContent = '';
  if (state.branch) branches.appendChild(button('All branches', 'btn quiet small', () => selectBranch(state.branch)));
  for (const branch of graph.branches || []) {
    const pick = button(branch.name, 'btn quiet small', () => selectBranch(branch.name));
    pick.setAttribute('aria-pressed', String(branch.name === state.branch));
    pick.title = branch.name === state.branch ? 'Show every branch' : 'Show only the history of ' + branch.name;
    branches.appendChild(pick);
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
    // Only the analysis has a badge: a plan is rarely run, and its gray
    // "no result" badge cluttered every commit.
    meta.appendChild(verdictChip(displayedRun(commit.sha, 'normal'), state.repoKey));
    const who = document.createElement('span'); who.className = 'note';
    who.textContent = [commit.author, commit.date ? timeAgo(commit.date) : ''].filter(Boolean).join(' · ');
    meta.appendChild(who);
    row.appendChild(meta);
    list.appendChild(row);
  }
  tree.appendChild(list);
}

// renderCommitActions offers "Run analysis" while the selected commit has no
// readable analysis result; once one exists, the report below is the whole view.
function renderCommitActions() {
  const repo = state.repos.get(state.repoKey);
  const holder = el('commit-actions');
  holder.textContent = '';
  if (!repo || !state.commit) return;
  const run = displayedRun(state.commit, 'normal');
  const available = run && !isPending(run) && !['failed', 'cancelled'].includes(run.status)
    && state.unreadable !== pendingKey(state.repoKey, state.commit, 'normal');
  holder.classList.toggle('hidden', Boolean(available));
  if (available) return;
  const launch = button(!run || !isPending(run) ? 'Run analysis' : run.status === 'running' ? 'Analysis running…' : 'Analysis queued…', 'btn', () => analyzeCommit());
  launch.id = 'run-analysis';
  launch.disabled = isPending(run);
  holder.appendChild(launch);
}

async function analyzeCommit() {
  const repoKey = state.repoKey, commit = state.commit;
  const key = pendingKey(repoKey, commit, 'normal');
  // Shown until the hub names the attempt; events or polling then take over.
  const optimistic = { status: 'queued', commit, variant: 'normal', repo_key: repoKey, requested: Date.now() };
  state.pending.set(key, optimistic);
  watchPending();
  renderCommitActions(); renderGraph();
  try {
    const queued = await api('/api/repos/' + encodeURIComponent(repoKey) + '/analyze', {
      method: 'POST', body: { commit, variant: 'normal' },
    });
    if (queued?.commit === commit && state.pending.get(key) === optimistic) state.pending.delete(key);
    if (!followQueued(repoKey, queued) && repoKey === state.repoKey) { renderCommitActions(); renderGraph(); }
    toast('Analysis queued for ' + shortSha(commit) + '.');
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

// renderReportCommit writes the head's first line: the repository, the commit
// and its title. It returns the commit's node of the graph, if loaded.
function renderReportCommit(repo, run) {
  const line = document.createElement('div');
  line.className = 'report-commit-line';
  const name = document.createElement('h2');
  name.id = 'report-repo';
  name.textContent = repo?.full_name || 'Select a repository';
  line.appendChild(name);
  el('report-head').appendChild(line);
  const commit = run?.commit || state.commit;
  if (!commit) return null;
  const node = (state.graphs.get(repo?.key)?.commits || []).find((c) => c.sha === commit) || null;
  const sha = document.createElement('code');
  sha.className = 'report-sha';
  sha.textContent = shortSha(commit);
  sha.title = commit;
  line.appendChild(sha);
  const heading = document.createElement('p');
  heading.id = 'selected-commit';
  heading.className = 'report-commit';
  heading.textContent = run?.message || node?.message || '';
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
  return node;
}

// renderCommitFacts shows what Git tells of a commit no analysis has read
// yet: its author and age, and the size of its change.
function renderCommitFacts(node) {
  const head = el('report-head');
  const sub = document.createElement('p');
  sub.id = 'report-sub';
  sub.className = 'report-sub';
  if (node) sub.textContent = [node.author && 'by ' + node.author, node.date && timeAgo(node.date)].filter(Boolean).join(' · ');
  head.appendChild(sub);
  if (!node?.stats) return;
  const stats = document.createElement('div');
  stats.className = 'stats';
  stats.appendChild(stat(node.stats.files, node.stats.files === 1 ? 'file' : 'files'));
  stats.appendChild(stat('+' + node.stats.additions + ' / -' + node.stats.deletions, 'lines'));
  if (node.parents && node.parents.length > 1) stats.appendChild(stat(node.parents.length, 'parents'));
  head.appendChild(stats);
}

function clearReport(repo) {
  state.view = null; state.run = null; state.feedback = null; state.replyTo = null; state.unreadable = null;
  el('report-head').textContent = '';
  renderCommitFacts(renderReportCommit(repo));
  el('filters').classList.add('hidden');
  el('alerts').textContent = '';
  el('extras').classList.add('hidden');
  el('report-empty').classList.remove('hidden');
  el('report-empty').textContent = 'No analysis for this commit yet.';
}

async function loadReport() {
  const repo = state.repos.get(state.repoKey);
  const commit = state.commit;
  if (!repo || !commit) return;
  clearReport(repo);
  const reportID = ++state.reportID;
  const run = cachedRun(commit, 'normal');
  if (!run) return;
  el('report-empty').textContent = 'Loading cached result…';
  try {
    const payload = await api('/api/repos/' + encodeURIComponent(repo.key)
      + '/reports/' + encodeURIComponent(commit));
    if (reportID !== state.reportID || repo.key !== state.repoKey || commit !== state.commit) return;
    state.view = payload.view;
    state.run = payload.run;
    renderReport();
    loadFeedback(repo.key, commit, reportID);
  } catch (err) {
    if (reportID !== state.reportID || repo.key !== state.repoKey || commit !== state.commit) return;
    el('report-empty').textContent = run.error || err.message;
    state.unreadable = pendingKey(repo.key, commit, 'normal');
    renderCommitActions();
  }
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

  renderReportCommit(repo, run);

  const title = document.createElement('div');
  title.className = 'report-title';
  title.id = 'report-verdict';
  renderVerdictLine(title);
  head.appendChild(title);

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

  if (view.pr_summary) head.appendChild(renderPRSummary(view.pr_summary));
  else if (view.pr_summary_error) {
    const missing = document.createElement('p');
    missing.className = 'report-sub';
    missing.textContent = 'AI report not written: ' + view.pr_summary_error + '. The alerts below are complete.';
    head.appendChild(missing);
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

// renderVerdictLine fills the head's verdict line: the CLI's verdict and,
// when it asks for a human review, the button that records one. A reviewed
// commit reads "Reviewed"; the CLI's request stays in the title.
function renderVerdictLine(line) {
  const view = state.view;
  const run = state.run;
  if (!view) return;
  line.textContent = '';
  const summary = view.summary;
  const verdict = document.createElement('span');
  verdict.className = 'verdict ' + (summary.verdict || 'failed');
  if (summary.verdict === 'review') verdict.classList.add(reviewTone(summary));
  verdict.textContent = verdictLabel(summary.verdict);
  const mark = summary.verdict === 'review' ? reviewMark(state.repoKey, state.commit) : null;
  if (mark) {
    verdict.className = 'verdict reviewed';
    verdict.textContent = 'Reviewed';
    verdict.title = 'Human review was required at ' + reviewLevel(summary) + ' level.';
  } else if (summary.verdict === 'review' && belowThreshold(summary)) {
    verdict.className = 'verdict below-threshold';
    verdict.textContent = 'Review below ' + LEVELS[state.minSeverity];
    verdict.title = 'Human review was requested at ' + reviewLevel(summary) + ' level, under the selected threshold.';
  }
  line.appendChild(verdict);
  if (run && run.status === 'failed') line.appendChild(chip('analysis failed', 'bad'));
  if (summary.verdict !== 'review' || (run && run.status !== 'done')) return;
  if (mark) {
    const by = document.createElement('span');
    by.className = 'note';
    by.textContent = 'by ' + mark.by + ' · ' + timeAgo(mark.at);
    line.appendChild(by);
  }
  const toggle = button(mark ? 'Mark as not reviewed' : 'Mark as reviewed', mark ? 'btn quiet small' : 'btn small', () => setReviewed(!mark, toggle));
  toggle.id = 'mark-reviewed';
  toggle.title = mark ? 'Withdraw the review mark: the commit asks for a human review again' : 'Record that you reviewed this commit';
  line.appendChild(toggle);
}

// setReviewed records, or withdraws, the review of the open report's commit.
async function setReviewed(reviewed, control) {
  const repoKey = state.repoKey;
  const commit = state.commit;
  control.disabled = true;
  try {
    const payload = await api('/api/repos/' + encodeURIComponent(repoKey) + '/reports/' + encodeURIComponent(commit) + '/review', {
      method: 'PUT',
      body: { reviewed },
    });
    upsertRepo(payload.repo);
    refreshReviewMarks(repoKey);
    toast(reviewed ? 'Marked ' + shortSha(commit) + ' as reviewed.' : 'Review mark withdrawn.');
  } catch (err) {
    toast(err.message, true);
    control.disabled = false;
  }
}

// refreshReviewMarks redraws what shows the review marks of a repository.
function refreshReviewMarks(repoKey) {
  renderReviewCount();
  if (repoKey !== state.repoKey) return;
  renderGraph();
  const line = el('report-verdict');
  if (line) renderVerdictLine(line);
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
    let count;
    if (kind.key === 'all') count = alerts.filter(aboveThreshold).length;
    else if (kind.key === 'everything') count = alerts.length;
    else count = alerts.filter((a) => a.kind === kind.key).length;
    if (kind.key !== 'all' && kind.key !== 'everything' && count === 0) continue;
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
  const alerts = state.view.alerts || [];
  if (state.kind === 'everything') return alerts;
  return alerts.filter((alert) => {
    if (!aboveThreshold(alert)) return false;
    if (state.kind !== 'all' && alert.kind !== state.kind) return false;
    return true;
  });
}

function aboveThreshold(alert) {
  return LEVELS.indexOf(alert.severity) >= state.minSeverity;
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
  if (state.view) renderReport();
}

function renderAlerts() {
  const list = el('alerts');
  list.textContent = '';
  // The summary's risks follow the same filters as the alerts; they are
  // rebuilt only when the filters changed, so that their open diffs stay.
  if (state.view && state.view.pr_summary) {
    for (const holder of document.querySelectorAll('.pr-rated')) {
      if (holder.dataset.filter !== ratedFilterKey()) renderRated(holder, state.view.pr_summary);
    }
  }
  const alerts = filteredAlerts();
  const total = state.view ? (state.view.alerts || []).length : 0;
  el('alert-count').textContent = alerts.length + ' of ' + total + ' alerts shown';

  const empty = () => {
    const item = document.createElement('li');
    item.className = 'empty';
    item.textContent = total === 0
      ? 'This run recorded no alert.'
      : 'No alert at this severity. Lower the filter to see the rest.';
    return item;
  };

  // With a summary, the list is the report laid out by change area: each
  // area with the alerts it cites, then the alerts the summary cites
  // nowhere. An alert only a risk cites lives under that risk, in the head.
  const areas = (state.view && state.view.pr_summary && state.view.pr_summary.changes) || [];
  if (areas.length === 0) {
    if (alerts.length === 0) list.appendChild(empty());
    for (const alert of alerts) list.appendChild(alertItem(alert, renderAlerts));
    return;
  }
  const all = state.view.alerts || [];
  areas.forEach((change, i) => {
    const own = alerts.filter((alert) => alert.area === i + 1);
    list.appendChild(areaItem(change, own.length, all.filter((alert) => alert.area === i + 1).length));
    for (const alert of own) list.appendChild(alertItem(alert, renderAlerts));
  });
  const inRisks = riskCitedAlerts(state.view.pr_summary);
  const other = alerts.filter((alert) => (!alert.area || alert.area > areas.length) && !inRisks.has(alert.id));
  if (other.length) {
    list.appendChild(otherAreaItem(other.length));
    for (const alert of other) list.appendChild(alertItem(alert, renderAlerts));
  }
  if (alerts.length === 0 && total > 0) list.appendChild(empty());
}

// alertItem renders one alert, folded or unfolded; rerender redraws the list
// that holds it after a click.
function alertItem(alert, rerender) {
  const item = document.createElement('li');
  item.className = 'alert';
  item.dataset.alertId = alert.id;

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
    if (state.feedback && alert.kind !== 'focus') item.appendChild(feedbackBlock(alert, rerender));
  }
  return item;
}

/* ------------------------------------------------------- team feedback -- */

function feedbackPath(repoKey, commit) {
  return '/api/repos/' + encodeURIComponent(repoKey) + '/reports/' + encodeURIComponent(commit) + '/feedback';
}

// loadFeedback fetches the votes, comments and replies on the open report.
// Feedback is optional: the report stays usable when it cannot be loaded.
async function loadFeedback(repoKey, commit, reportID) {
  try {
    const payload = await api(feedbackPath(repoKey, commit));
    if (reportID !== state.reportID) return;
    state.feedback = { commit, learning: payload.learning, entries: payload.entries || [] };
    renderAlerts();
    renderExtras();
  } catch (err) {
    // Nothing to show: feedback controls stay hidden.
  }
}

async function sendFeedback(alert, body, rerender) {
  const feedback = state.feedback;
  if (!feedback) return;
  try {
    const payload = await api(feedbackPath(state.repoKey, feedback.commit), {
      method: 'POST',
      body: Object.assign({ alert_id: alert.id }, body),
    });
    if (state.feedback !== feedback) return;
    feedback.entries = payload.feedback.entries || [];
    feedback.learning = payload.feedback.learning;
    state.replyTo = null;
    rerender();
    toast(body.comment ? 'Comment saved.' : 'Thanks, your vote was saved.');
  } catch (err) {
    toast(err.message, true);
  }
}

// feedbackBlock lets the team vote on a finding, comment on it and reply to
// comments. The hub turns these reactions, with what developers did after
// each finding, into guidance for the AI reviewer of later reviews.
function feedbackBlock(alert, rerender) {
  const entries = state.feedback.entries.filter((e) => e.alert_id === alert.id);
  const me = state.me?.user?.login || '';
  const votes = new Map();
  for (const e of entries) if (e.vote) votes.set(e.author, e.vote);
  let up = 0, down = 0;
  for (const vote of votes.values()) vote === 'up' ? up++ : down++;
  const mine = votes.get(me) || '';

  const box = document.createElement('div');
  box.className = 'feedback';
  const row = document.createElement('div');
  row.className = 'row';
  const question = document.createElement('span');
  question.className = 'note';
  question.textContent = 'Was this finding useful?';
  row.appendChild(question);
  for (const [vote, label, count] of [['up', '👍 Useful', up], ['down', '👎 Not useful', down]]) {
    const b = button(label + (count ? ' · ' + count : ''), 'btn quiet small' + (mine === vote ? ' active' : ''), (event) => {
      event.stopPropagation();
      sendFeedback(alert, { vote }, rerender);
    });
    b.setAttribute('aria-pressed', mine === vote ? 'true' : 'false');
    b.dataset.vote = vote;
    row.appendChild(b);
  }
  box.appendChild(row);

  const comments = entries.filter((e) => e.comment);
  const list = document.createElement('ul');
  list.className = 'feedback-comments';
  const appendComment = (e, depth) => {
    const item = document.createElement('li');
    item.className = 'feedback-comment';
    item.style.marginLeft = (depth * 16) + 'px';
    const head = document.createElement('div');
    head.className = 'note';
    head.textContent = e.author + (e.vote === 'up' ? ' · useful' : e.vote === 'down' ? ' · not useful' : '')
      + ' · ' + new Date(e.at).toLocaleString();
    item.appendChild(head);
    const text = document.createElement('div');
    text.textContent = e.comment;
    item.appendChild(text);
    const reply = button('Reply', 'btn quiet small', (event) => {
      event.stopPropagation();
      state.replyTo = { alert: alert.id, id: e.id, author: e.author };
      rerender();
    });
    reply.dataset.reply = e.id;
    item.appendChild(reply);
    list.appendChild(item);
    for (const child of comments.filter((c) => c.reply_to === e.id)) appendComment(child, Math.min(depth + 1, 4));
  };
  const known = new Set(comments.map((e) => e.id));
  for (const e of comments.filter((c) => !c.reply_to || !known.has(c.reply_to))) appendComment(e, 0);
  if (comments.length) box.appendChild(list);

  const replying = state.replyTo && state.replyTo.alert === alert.id ? state.replyTo : null;
  if (replying) {
    const note = document.createElement('div');
    note.className = 'row note';
    note.textContent = 'Replying to ' + replying.author + ' ';
    note.appendChild(button('Cancel', 'btn quiet small', (event) => {
      event.stopPropagation();
      state.replyTo = null;
      rerender();
    }));
    box.appendChild(note);
  }
  const text = document.createElement('textarea');
  text.className = 'search feedback-text';
  text.rows = 2;
  text.maxLength = 2000;
  text.placeholder = replying ? 'Your reply' : 'Tell the reviewer what your team expects here (optional)';
  text.setAttribute('aria-label', replying ? 'Reply' : 'Comment on this finding');
  text.addEventListener('click', (event) => event.stopPropagation());
  box.appendChild(text);
  const send = button(replying ? 'Send reply' : 'Comment', 'btn small', (event) => {
    event.stopPropagation();
    const comment = text.value.trim();
    if (!comment) { toast('Write a comment first.', true); return; }
    sendFeedback(alert, replying ? { comment, reply_to: replying.id } : { comment }, rerender);
  });
  send.dataset.feedbackSend = 'true';
  box.appendChild(send);
  if (!state.feedback.learning) {
    const off = document.createElement('p');
    off.className = 'note';
    off.textContent = 'Learning is off for this repository: feedback is kept but future reviews do not use it.';
    box.appendChild(off);
  }
  return box;
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

// renderExtras shows what is deliberately not an alert: coverage and
// everything the run left unverified.
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
      if (JSON.stringify(previous?.reviewed || {}) !== JSON.stringify(event.repo.reviewed || {})) refreshReviewMarks(event.repo.key);
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
    if (refreshHistory && state.repoKey) {
      state.graphs.delete(state.repoKey);
      await loadHistory();
    } else if (state.repoKey) {
      await refreshCommitTree();
    }
    renderDashboard();
  } catch (err) {
    toast('Could not refresh results: ' + err.message, true);
    renderRepos(); // Still age the current snapshot while offline.
  } finally {
    refreshingDashboard = false;
  }
}

// refreshCommitTree reloads the commit tree and the run history of the
// selected repository in place, every minute with the dashboard: new commits
// and results appear without disturbing the open report, the selection or the
// scroll position. A failure keeps the current tree; the next tick retries.
async function refreshCommitTree() {
  const repo = state.repos.get(state.repoKey);
  if (!repo || !state.graphs.has(repo.key)) return;
  const loadID = state.loadID;
  const graphBranch = state.branch;
  const base = '/api/repos/' + encodeURIComponent(repo.key);
  let graph, runs;
  try {
    [graph, runs] = await Promise.all([api(commitsPath(repo)), api(base + '/runs')]);
  } catch (err) {
    return;
  }
  // A repository or commit selected meanwhile has loaded its own history.
  if (loadID !== state.loadID || repo.key !== state.repoKey || graphBranch !== state.branch) return;
  state.graphs.set(repo.key, graph);
  state.runs = runs.runs || [];
  settleFromHistory(repo.key);
  const tree = el('commit-tree');
  const top = tree.scrollTop, left = tree.scrollLeft;
  renderGraph();
  tree.scrollTop = top;
  tree.scrollLeft = left;
  if (state.commit) renderCommitActions();
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
  // Each item of the Settings menu closes it and opens its dialog.
  const settingsMenu = el('settings-menu');
  for (const [id, open] of [['analyses', openActivityDialog], ['agent-access', openAgentDialog], ['language', openLanguageDialog]]) {
    el(id).addEventListener('click', () => { settingsMenu.open = false; open(); });
  }
  document.addEventListener('click', (event) => { if (!settingsMenu.contains(event.target)) settingsMenu.open = false; });
  settingsMenu.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && settingsMenu.open) { settingsMenu.open = false; el('settings').focus(); }
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
  el('modal').addEventListener('click', (event) => {
    if (event.target === el('modal')) closeModal();
  });
  document.addEventListener('keydown', (event) => {
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
