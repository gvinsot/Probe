'use strict';

const fixtureSHA = (letter) => letter.repeat(40);
const fixtureRepo = { key: 'repo', full_name: 'acme/shop', provider: 'github', default_branch: 'main', web_url: 'https://github.com/acme/shop', has_policy: true, admin: true };
const fixtureCounts = { total: 1, high: 1, critical: 0, medium: 0, low: 0 };
const fixtureRun = (variant) => ({ commit: fixtureSHA('a'), variant, status: 'done', mode: variant === 'plan' ? 'plan' : 'review-read-only', summary: { verdict: 'review', counts: fixtureCounts, reproduced: 0, unverified: 1, focused_lines: 2, changed_lines: 5, changed_files: 1, additions: 4, deletions: 1 } });
const fixtureAgo = (hours) => new Date(Date.now() - hours * 3600 * 1000).toISOString();
fixtureRepo.recent = [
  Object.assign(fixtureRun('normal'), { queued_at: fixtureAgo(2) }),
  { commit: fixtureSHA('e'), variant: 'normal', status: 'done', queued_at: fixtureAgo(80), summary: { verdict: 'blocked', counts: { total: 2, critical: 1, high: 1, medium: 0, low: 0 } } },
];
const fixtureCalls = [];
let fixtureStream;
let fixtureActivityError = false;
let fixtureActivityGate;
let fixtureActivities = [
  { repo_key: 'repo', commit: fixtureSHA('a'), variant: 'normal', status: 'queued', queued_at: fixtureAgo(1) },
  { repo_key: 'repo', commit: fixtureSHA('c'), variant: 'plan', status: 'running', queued_at: fixtureAgo(2), started_at: fixtureAgo(1) },
  { repo_key: 'repo', commit: fixtureSHA('a'), variant: 'normal', status: 'failed', queued_at: fixtureAgo(3), finished_at: fixtureAgo(2), error: '<img src=x onerror=alert(1)>' },
];
let fixtureFeedback = [];
let fixtureNewCommits = [];
let fixtureReposGate;
let fixtureReportError = false;
let fixtureRuns = [fixtureRun('normal'), fixtureRun('plan')];
window.EventSource = class { constructor() { fixtureStream = this; } };
window.fetch = async (path, init) => {
  fixtureCalls.push({ path, init });
  let data;
  if (path === '/api/me') data = { authenticated: true, mode: 'review-read-only', csrf: 'csrf', user: { login: 'octocat', provider: 'github' }, settings: { report_language: 'English' }, report_languages: ['English', 'French', 'German'] };
  else if (path === '/api/settings') data = { settings: { report_language: JSON.parse(init.body).report_language } };
  else if (path === '/api/analyses') {
    if (fixtureActivityGate) await fixtureActivityGate;
    if (fixtureActivityError) throw new Error('temporary failure');
    data = { analyses: fixtureActivities };
  }
  else if (path === '/api/repos') {
    if (fixtureReposGate) await fixtureReposGate;
    data = { repos: [fixtureRepo], recent_limit: 7 };
  }
  else if (path.endsWith('/policy')) {
    const body = JSON.parse(init.body);
    const key = path.split('/')[3];
    data = body.preview ? { language: 'go', policy: '{"version":1}' }
      : { repo: { ...state.repos.get(key), has_policy: true } };
  }
  else if (path.endsWith('/rules')) {
    const key = path.split('/')[3];
    const rules = JSON.parse(init.body).rules.trim();
    data = { repo: { ...state.repos.get(key), coding_rules: rules || undefined } };
  }
  else if (path.endsWith('/monitor')) {
    const key = path.split('/')[3];
    data = { repo: { ...state.repos.get(key), monitored: init.method === 'POST' } };
  }
  else if (path.endsWith('/review') && init && init.method === 'PUT') {
    const key = path.split('/')[3];
    const commit = path.split('/')[5];
    const reviewed = JSON.parse(init.body).reviewed ? { [commit]: { by: 'octocat', at: new Date().toISOString() } } : undefined;
    data = { repo: { ...state.repos.get(key), reviewed } };
  }
  else if (path.endsWith('/reviews')) data = { reviews: [
    { commit: fixtureSHA('a'), message: 'Merge feature', verdict: 'review', reviewed: false, by: 'octocat', at: new Date().toISOString() },
    { commit: fixtureSHA('a'), message: 'Merge feature', verdict: 'review', reviewed: true, by: 'octocat', at: new Date().toISOString() },
  ] };
  else if (path.endsWith('/commits?branch=feature%2Fui')) data = { limited: false, branches: [{name:'main',sha:fixtureSHA('a')},{name:'feature/ui',sha:fixtureSHA('c')}], commits: [
    { sha: fixtureSHA('c'), parents: [fixtureSHA('d')], branches: ['feature/ui'], message: '<img src=x onerror=alert(1)>', author: 'Grace' },
    { sha: fixtureSHA('d'), parents: [], branches: [], message: 'Initial commit', author: 'Ada' },
  ] };
  else if (path.includes('/commits?branch=')) throw new Error('this branch no longer exists');
  else if (path.endsWith('/commits')) data = { limited: false, branches: [{name:'main',sha:fixtureSHA('a')},{name:'feature/ui',sha:fixtureSHA('c')}], commits: [
    ...fixtureNewCommits,
    { sha: fixtureSHA('a'), parents: [fixtureSHA('b'), fixtureSHA('c')], branches: ['main'], message: 'Merge feature', author: 'Ada' },
    { sha: fixtureSHA('c'), parents: [fixtureSHA('d')], branches: ['feature/ui'], message: '<img src=x onerror=alert(1)>', author: 'Grace', stats: { files: 3, additions: 12, deletions: 4 } },
    { sha: fixtureSHA('b'), parents: [fixtureSHA('d')], branches: [], message: 'Main branch work', author: 'Ada' },
    { sha: fixtureSHA('d'), parents: [], branches: [], message: 'Initial commit', author: 'Ada' },
  ] };
  else if (path.includes('/reports/') && path.endsWith('/feedback')) {
    if (init && init.method === 'POST') {
      const body = JSON.parse(init.body);
      fixtureFeedback.push({ id: 'fb-' + fixtureFeedback.length, alert_id: body.alert_id, vote: body.vote || '', comment: body.comment || '', reply_to: body.reply_to || '', author: 'octocat', at: new Date().toISOString() });
      data = { entry: fixtureFeedback.at(-1), feedback: { learning: true, entries: fixtureFeedback.slice() } };
    } else data = { learning: true, entries: fixtureFeedback.slice() };
  }
  else if (path.endsWith('/feedback')) data = { repo: state.repos.get(path.split('/')[3]), feedback: { topics: [], comments: [] } };
  else if (path.endsWith('/learning')) {
    const key = path.split('/')[3];
    const learned = { topics: [{ topic: 'signal:no_test_change', useful: 0, not_useful: 3, changed: 1, unchanged: 4 }], comments: [] };
    data = init && init.method === 'PUT'
      ? { repo: { ...state.repos.get(key), learning: JSON.parse(init.body).enabled }, feedback: learned }
      : { learning: true, feedback: learned };
  }
  else if (path.includes('/runs')) data = { runs: fixtureRuns };
  else if (path.includes('/reports/')) {
    if (fixtureReportError) throw new Error('cached result unavailable');
    const variant = new URL(path, location.origin).searchParams.get('variant');
    const run = fixtureRun(variant);
    data = { run, view: { summary: run.summary, alerts: [], files: [], unverified: ['No checks ran'], plan_drift: variant === 'plan' ? { status: 'conforming', decision: 'human_review_required', decision_reasons: ['No checks ran'] } : null }, plan: variant === 'plan' ? { proposal: { summary: 'Generated plan' } } : null };
  } else if (path.endsWith('/cancel')) {
    const body = JSON.parse(init.body);
    data = { status: 'cancelled', commit: body.commit, variant: body.variant };
  } else if (path.endsWith('/rerun')) {
    const body = JSON.parse(init.body);
    data = { status: 'queued', commit: body.commit, variant: body.variant, queued_at: new Date().toISOString() };
  } else if (path.endsWith('/analyze')) {
    const body = JSON.parse(init.body);
    data = { status: 'queued', commit: body.commit || fixtureSHA('a'), variant: body.variant || 'normal', queued_at: new Date().toISOString() };
  }
  else throw new Error('Unexpected request: ' + path);
  return { ok: true, status: 200, text: async () => JSON.stringify(data) };
};

function fixtureFail(error) {
  document.body.dataset.testResult = 'FAIL: ' + (error.stack || error);
}
window.addEventListener('error', (event) => fixtureFail(event.error));
window.addEventListener('unhandledrejection', (event) => fixtureFail(event.reason));
window.addEventListener('DOMContentLoaded', async () => {
  const settle = () => new Promise((resolve) => setTimeout(resolve, 30));
  const assert = (condition, message) => { if (!condition) throw new Error(message); };
  try {
    await settle();
    // Jobs queue, MCP and Language share the Settings menu.
    const settingsMenu = document.getElementById('settings-menu');
    assert([...settingsMenu.querySelectorAll('.menu-item')].map((b) => b.textContent).join('|') === 'Jobs queue|MCP|Language' && !document.querySelector('.topbar #sync') && document.querySelector('.repo-panel .panel-head #sync').getAttribute('aria-label') === 'Refresh' && el('sync').querySelector('svg') && el('refresh-commits').querySelector('svg') && !el('sync').textContent.trim(), 'settings menu in the top bar, repository refresh at the head of the Repositories column');
    document.getElementById('settings').click();
    await settle();
    assert(settingsMenu.open, 'the menu opens');
    document.getElementById('language').click();
    await settle();
    assert(!settingsMenu.open && document.getElementById('modal-title').textContent === 'Language', 'an item closes the menu and opens its dialog');
    const languageSelect = document.getElementById('report-language');
    assert(languageSelect.value === 'English' && languageSelect.options.length === 3, 'the account language is preselected among the choices');
    languageSelect.value = 'French';
    document.getElementById('report-language-save').click();
    await settle();
    const settingsCall = fixtureCalls.find((call) => call.path === '/api/settings');
    assert(settingsCall && settingsCall.init.method === 'PUT' && JSON.parse(settingsCall.init.body).report_language === 'French' && settingsCall.init.headers['X-Probe-CSRF'] === 'csrf', 'saving puts the language with CSRF');
    assert(document.getElementById('modal').classList.contains('hidden') && state.me.settings.report_language === 'French', 'the saved language is kept');
    const activityButton = document.getElementById('analyses');
    activityButton.click();
    await settle();
    const activityText = () => document.getElementById('activity-list').textContent;
    assert(activityText().includes('Queued (1)') && activityText().includes('Running (1)') && activityText().includes('Past analyses (1)'), 'all activity groups rendered');
    assert(activityText().includes('acme/shop') && activityText().includes('Plan') && activityText().includes('Failed'), 'repository, variant and failure visible');
    assert(document.querySelectorAll('#activity-list img').length === 0 && activityText().includes('<img'), 'activity error rendered as text');
    fixtureActivities[0].status = 'done';
    fixtureActivities[0].finished_at = fixtureAgo(0);
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { ...fixtureRun('normal'), queued_at: fixtureAgo(1) } }) });
    await settle();
    assert(activityText().includes('Queued (0)') && activityText().includes('Past analyses (2)'), 'activity updates after a run event');
    fixtureActivityError = true;
    document.querySelector('#modal-footer button').click();
    await settle();
    assert(document.getElementById('activity-status').textContent.includes('temporary failure') && activityText().includes('Past analyses (2)'), 'refresh failure preserves previous results');
    fixtureActivityError = false;
    fixtureActivities = [];
    document.querySelector('#modal-footer button').click();
    await settle();
    assert(activityText().includes('Queued (0)') && activityText().includes('No analyses.'), 'empty history rendered');
    document.getElementById('modal-close').focus();
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, bubbles: true }));
    assert(document.activeElement === document.querySelector('#modal-footer button'), 'modal focus stays inside');
    let releaseActivity;
    fixtureActivityGate = new Promise((resolve) => { releaseActivity = resolve; });
    document.querySelector('#modal-footer button').click();
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    assert(document.getElementById('modal').classList.contains('hidden') && document.activeElement === document.getElementById('settings'), 'Escape closes modal and returns focus to the Settings menu');
    document.getElementById('modal-body').textContent = 'Another dialog';
    releaseActivity();
    fixtureActivityGate = null;
    await settle();
    assert(document.getElementById('modal-body').textContent === 'Another dialog', 'late activity response cannot replace another dialog');
    const repoPanel = document.querySelector('.repo-panel');
    assert(getComputedStyle(repoPanel).position === 'sticky', 'repository panel does not scroll with the page');
    assert(getComputedStyle(document.getElementById('repos')).overflowY === 'auto', 'repository list scrolls on its own');
    assert(parseFloat(getComputedStyle(repoPanel).top) >= document.querySelector('.topbar').getBoundingClientRect().height, 'repository panel stays below the top bar: ' + document.querySelector('.topbar').getBoundingClientRect().height);
    // Policy filtering composes with name and monitoring filters.
    const missingRepo = { ...fixtureRepo, key: 'missing', full_name: 'acme/missing', has_policy: false };
    const monitoredRepo = { ...fixtureRepo, key: 'monitored', full_name: 'acme/watched', monitored: true };
    state.repos.set(missingRepo.key, missingRepo);
    state.repos.set(monitoredRepo.key, monitoredRepo);
    renderRepos();
    const names = () => Array.from(document.querySelectorAll('.repo-name'), (node) => node.textContent);
    const hasPolicy = document.getElementById('only-policy');
    const missingPolicy = document.getElementById('only-nopolicy');
    const monitored = document.getElementById('only-monitored');
    hasPolicy.click();
    assert(names().length === 2 && !names().includes('acme/missing'), 'Has policy hides repositories without a policy');
    monitored.click();
    assert(names().join() === 'acme/watched', 'Has policy combines with Monitored only');
    monitored.click();
    const search = document.getElementById('repo-search');
    search.value = 'SHOP'; search.dispatchEvent(new Event('input'));
    assert(names().join() === 'acme/shop', 'Has policy combines with name search');
    search.value = ''; search.dispatchEvent(new Event('input'));
    missingPolicy.click();
    assert(!hasPolicy.checked && names().join() === 'acme/missing', 'Missing policy clears Has policy');
    hasPolicy.click();
    assert(!missingPolicy.checked && names().length === 2, 'Has policy clears Missing policy');
    hasPolicy.click();
    assert(names().length === 3, 'clearing policy filters restores every repository');

    // Adding a policy never requests monitoring, even for an administrator.
    for (const admin of [true, false]) {
      state.repos.set('missing', { ...missingRepo, admin });
      renderRepos();
      const setup = document.querySelector('#repos .setup');
      assert(setup.textContent === 'Add policy', 'repository without a policy offers Add policy');
      const callStart = fixtureCalls.length;
      setup.click();
      await settle();
      assert(document.getElementById('modal-title').textContent === 'Add policy to acme/missing', 'policy dialog title matches the action');
      const policyText = document.getElementById('policy-text');
      assert(policyText.value === '{"version":1}' && !policyText.disabled, 'policy preview rendered, editable');
      const create = document.querySelector('#modal-footer .btn:not(.quiet)');
      assert(create.textContent === 'Commit the policy' && !create.disabled, 'policy can be committed after preview');
      // The administrator edits the policy; the other commits it as generated.
      const edit = (text) => { policyText.value = text; policyText.dispatchEvent(new Event('input')); };
      if (admin) {
        edit('{"version":1,');
        assert(create.disabled && el('policy-problem').textContent.startsWith('Invalid JSON'), 'invalid JSON cannot be committed');
        edit('[1]');
        assert(create.disabled && el('policy-problem').textContent === 'The policy must be a JSON object.', 'only an object can be committed');
        edit('{"version":1,"timeout_seconds":60}');
        assert(!create.disabled && el('policy-problem').textContent === '', 'a valid edit can be committed');
      }
      create.click();
      await settle();
      const calls = fixtureCalls.slice(callStart);
      assert(calls.length === 2 && calls.every((call) => call.path.endsWith('/policy')), 'adding policy only requests preview and policy commit');
      assert(JSON.parse(calls[0].init.body).preview && !JSON.parse(calls[1].init.body).preview, 'preview precedes policy commit');
      assert(JSON.parse(calls[1].init.body).policy === (admin ? '{"version":1,"timeout_seconds":60}' : ''), 'an edited policy is sent, an unchanged one left to the hub');
      assert(state.repos.get('missing').has_policy && !state.repos.get('missing').monitored, 'policy creation leaves monitoring disabled');
      assert(document.getElementById('modal').classList.contains('hidden'), 'successful policy creation closes the dialog');
      const row = Array.from(document.querySelectorAll('#repos .repo')).find((node) => node.textContent.includes('acme/missing'));
      const monitor = row.querySelector('.monitor');
      assert(!row.querySelector('.setup') && monitor.textContent === 'Activate monitoring' && monitor.disabled === !admin, 'monitoring becomes a separate action with existing permissions');
      if (admin) {
        monitor.click();
        await settle();
        assert(state.repos.get('missing').monitored && fixtureCalls.at(-1).path.endsWith('/monitor'), 'explicit activation still enables monitoring');
        const settings = Array.from(document.querySelectorAll('#repos .repo')).find((node) => node.textContent.includes('acme/missing'))
          .querySelector('.icon-btn');
        assert(settings && !settings.parentNode.textContent.includes('Stop monitoring'), 'stop monitoring lives in the review settings dialog');
        settings.click();
        await settle();
        const stop = document.getElementById('stop-monitoring');
        assert(stop && stop.textContent === 'Stop monitoring', 'the review settings dialog offers to stop monitoring');
        stop.click();
        await settle();
        assert(!state.repos.get('missing').monitored && fixtureCalls.at(-1).init.method === 'DELETE' && el('modal').classList.contains('hidden'), 'stopping from the dialog disables monitoring');
      }
    }
    state.repos.delete('missing');
    state.repos.delete('monitored');
    renderRepos();
    document.querySelector('.repo-name').click();
    await settle();
    assert(document.querySelectorAll('.commit-row').length === 4, 'all graph commits rendered');
    assert(document.querySelectorAll('.graph-node').length === 4, 'graph nodes rendered');
    assert(document.getElementById('commit-tree').textContent.includes('feature/ui'), 'branch tip visible');
    assert(document.querySelectorAll('#commit-tree img').length === 0, 'commit content must be text');
    assert(document.querySelectorAll('#commit-tree .chip.unknown').length === 3, 'gray unknown badge for unanalyzed commits');
    assert(document.getElementById('commit-tree').textContent.includes('Human review required'), 'cached verdict badge');
    assert(!document.getElementById('commit-tree').textContent.includes('Normal'), 'the analysis badge has no mode prefix');
    assert(document.querySelector('#commit-tree .chip.warn').classList.contains('tone-high'), 'review badge tinted by the most severe alert');
    assert(document.querySelectorAll('.commit-row .commit-meta .chip').length === 4, 'one badge per commit, none for plans');
    assert(fixtureCalls.every((call) => !call.path.endsWith('/analyze')), 'browsing must not run analyses');
    const firstCommit = document.querySelector('.commit-open');
    assert(!firstCommit.textContent.includes('aaaaaaaa') && firstCommit.title.includes(fixtureSHA('a')), 'commit id only on hover');
    assert(!document.getElementById('commit-tree').textContent.includes('Parents'), 'parents are drawn, not listed');
    assert(document.querySelector('.commit-row .chip.branch').textContent === 'main', 'branch name in the tree');
    // A branch button reloads the tree with that branch only.
    const branchButton = (name) => [...document.querySelectorAll('#branches button')].find((b) => b.textContent === name);
    branchButton('feature/ui').click();
    await settle();
    assert(fixtureCalls.some((call) => call.path === '/api/repos/repo/commits?branch=feature%2Fui'), 'the branch is requested alone');
    assert(document.querySelectorAll('.commit-row').length === 2 && branchButton('feature/ui').getAttribute('aria-pressed') === 'true' && document.getElementById('graph-note').textContent.startsWith('Branch feature/ui only'), 'the tree shows one branch');
    branchButton('All branches').click();
    await settle();
    assert(document.querySelectorAll('.commit-row').length === 4 && !branchButton('All branches') && branchButton('feature/ui').getAttribute('aria-pressed') === 'false', 'All branches restores the whole tree');
    await selectBranch('gone');
    await settle();
    assert(state.branch === null && document.querySelectorAll('.commit-row').length === 4, 'a deleted branch falls back to every branch');
    const repoHead = document.querySelector('.repo-head');
    assert(!repoHead.textContent.includes('Analyze now') && repoHead.textContent.includes('Activate monitoring'), 'repository actions next to the name, without Analyze now');
    assert(!document.getElementById('repos').textContent.includes('.probe.json'), 'no policy tag');
    for (const id of ['jump-from-repos', 'jump-from-commits']) {
      assert(getComputedStyle(document.getElementById(id)).display === 'none', id + ' only on mobile');
    }
    const scrolls = [];
    const realScrollTo = window.scrollTo;
    window.scrollTo = (options) => scrolls.push(options.top);
    const expectedTop = (node) => Math.max(0, node.getBoundingClientRect().top + window.scrollY - document.querySelector('.topbar').offsetHeight - 8);
    document.getElementById('jump-from-repos').click();
    document.getElementById('jump-from-commits').click();
    window.scrollTo = realScrollTo;
    assert(scrolls[0] === expectedTop(document.getElementById('commit-browser')), 'repositories jump to the commit tree');
    assert(scrolls[1] === expectedTop(document.getElementById('report-pane')), 'commit tree jumps to the report');
    const splitter = document.getElementById('splitter');
    const column = document.getElementById('commit-browser');
    assert(!splitter.classList.contains('hidden'), 'splitter shown with the commit tree');
    const before = column.getBoundingClientRect().width;
    splitter.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    assert(column.getBoundingClientRect().width > before, 'splitter widens the commit tree');
    assert(localStorage.getItem('probe.commitColumnWidth') === splitter.getAttribute('aria-valuenow'), 'splitter width remembered');
    const x = splitter.getBoundingClientRect().left;
    splitter.dispatchEvent(new PointerEvent('pointerdown', { button: 0, clientX: x, pointerId: 1, bubbles: true }));
    splitter.dispatchEvent(new PointerEvent('pointermove', { clientX: x + 100, pointerId: 1, bubbles: true }));
    splitter.dispatchEvent(new PointerEvent('pointerup', { clientX: x + 100, pointerId: 1, bubbles: true }));
    assert(Math.round(column.getBoundingClientRect().width) === Math.round(before + 20 + 100), 'dragging the splitter resizes the commit tree');
    splitter.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    assert(Math.round(column.getBoundingClientRect().width) === 340, 'double-click resets the width');
    document.querySelectorAll('.commit-open')[1].click();
    await settle();
    assert(document.getElementById('report-head').textContent.includes('cccccccc'), 'uncached commit stays selected');
    assert(document.getElementById('filters').classList.contains('hidden'), 'previous report cleared');
    // Without an analysis, Git still tells the commit's size, on the line of
    // the repository, commit and title.
    const pendingLine = document.querySelector('#report-head .report-commit-line');
    assert(pendingLine.querySelector('#report-repo') && pendingLine.querySelector('.report-sha').textContent === 'cccccccccc' && pendingLine.querySelector('#selected-commit').textContent === '<img src=x onerror=alert(1)>', 'repository, commit and title on one line');
    const facts = [...document.querySelectorAll('#report-head .stat')].map((s) => s.textContent).join('|');
    assert(facts === '3files|+12 / -4lines' && document.getElementById('report-sub').textContent.includes('by Grace'), 'commit facts before any analysis: ' + facts);
    // Without an analysis, the commit only offers to run one: no Analysis or Plan cards.
    const commitActions = document.getElementById('commit-actions');
    assert(!commitActions.classList.contains('hidden') && commitActions.querySelectorAll('button').length === 1, 'a single action for an unanalyzed commit');
    assert(!document.getElementById('plan-intent') && !document.getElementById('report-dialog') && !document.querySelector('.comparison-card'), 'no Analysis or Plan cards');
    document.getElementById('run-analysis').click();
    await settle();
    const requests = fixtureCalls.filter((call) => call.path.endsWith('/analyze'));
    assert(requests.length === 1, 'the analysis is launched');
    const body = JSON.parse(requests[0].init.body);
    assert(body.commit === fixtureSHA('c') && body.variant === 'normal', 'exact selected commit and variant');
    assert(requests[0].init.headers['X-Probe-CSRF'] === 'csrf', 'analysis includes CSRF');
    const pending = state.pending.get(pendingKey('repo', fixtureSHA('c'), 'normal'));
    assert(pending && pending.status === 'queued' && runTimestamp(pending.queued_at), 'launched attempt followed by its server enqueue time');
    assert(document.getElementById('run-analysis').disabled && document.getElementById('run-analysis').textContent === 'Analysis queued…', 'the button shows the queued attempt');
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { commit: fixtureSHA('c'), variant: 'normal', status: 'done', queued_at: pending.queued_at } }) });
    assert(!state.pending.has(pendingKey('repo', fixtureSHA('c'), 'normal')), 'a finished event closes the attempt');
    state.recent.get('repo').delete(fixtureSHA('c')); // Keep the period fixtures below unchanged.
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'report', repo_key: 'repo', commit: fixtureSHA('a'), run: fixtureRun('normal') }) });
    await settle();
    assert(document.querySelectorAll('.commit-row:first-child .commit-meta .chip').length === 1, 'live results keep reviews aggregated');
    assert(document.getElementById('report-head').textContent.includes('cccccccc'), 'live results do not steal selection');
    document.querySelector('.commit-open').click();
    await settle();
    assert(commitActions.classList.contains('hidden') && !document.getElementById('run-analysis'), 'an analyzed commit shows its report only');
    assert(!document.getElementById('filters').classList.contains('hidden'), 'cached report shown on commit click');
    assert(document.getElementById('filters').firstElementChild.textContent === 'Details' && document.getElementById('filters').firstElementChild.nextElementSibling.querySelector('#kinds'), 'a Details title precedes the alert filters');
    const commitLine = document.querySelector('#report-head .report-commit-line');
    assert(commitLine.querySelector('#selected-commit') && commitLine.querySelector('a').textContent === 'Open the commit', 'Open the commit beside the commit title');
    assert(!document.getElementById('report-head').textContent.includes('never approves'), 'no disclaimer line');
    assert(document.querySelector('#report-head .verdict').textContent === 'Human review required', 'report verdict rendered');
    assert(document.querySelector('#report-head .verdict').classList.contains('tone-high'), 'report verdict tinted by the most severe alert');
    // "Mark as reviewed" turns the request into a recorded human review, everywhere.
    const reviewCountBefore = el('review-count').textContent;
    assert(el('mark-reviewed').textContent === 'Mark as reviewed', 'a review report offers the Mark as reviewed button');
    el('mark-reviewed').click();
    await settle();
    const reviewCall = fixtureCalls.find((call) => call.path === '/api/repos/repo/reports/' + fixtureSHA('a') + '/review');
    assert(reviewCall && reviewCall.init.method === 'PUT' && JSON.parse(reviewCall.init.body).reviewed === true && reviewCall.init.headers['X-Probe-CSRF'] === 'csrf', 'Mark as reviewed puts the mark with CSRF');
    assert(document.querySelector('#report-head .verdict').textContent === 'Reviewed' && document.getElementById('report-verdict').textContent.includes('by octocat') && el('mark-reviewed').textContent === 'Mark as not reviewed', 'the report reads Reviewed, by whom');
    assert(document.querySelector('.commit-row.selected .commit-meta .chip').textContent === 'reviewed' && !document.querySelector('.commit-row.selected').textContent.includes('Human review required'), 'the tree badge reads reviewed');
    assert(el('review-count').textContent !== reviewCountBefore, 'the review count drops');
    el('mark-reviewed').click();
    await settle();
    assert(document.querySelector('#report-head .verdict').textContent === 'Human review required' && el('mark-reviewed').textContent === 'Mark as reviewed' && el('review-count').textContent === reviewCountBefore, 'withdrawing the mark asks for a review again');
    assert(document.getElementById('mode-label').textContent.includes('AI review (read-only)'), 'deployment mode shown');
    assert(document.getElementById('report-head').textContent.includes('no code or tests were executed'), 'read-only report scope shown');
    const actualMode = state.run.mode;
    state.run.mode = 'lint'; renderReport();
    assert(!document.getElementById('report-head').textContent.includes('Read-only AI review'), 'old lint report keeps its own scope');
    state.run.mode = actualMode; renderReport();
    const plainView = state.view;
    state.view = Object.assign({}, plainView, {
      summary: Object.assign({}, plainView.summary, { suspicions: 1, dismissed: 1 }),
      reviewer_summary: '<img src=x onerror=alert(1)> Main risk: refunds.',
      alerts: [{ id: 'signal:s1', kind: 'signal', severity: 'high', title: 'Refunds are no longer checked', original_title: 'validation removed', explanation: 'The guard was removed.', judgment: 'risk', status: 'OBSERVED', path: 'pay.go', line: 3, end_line: 3, side: 'new' }, { id: 'signal:s3', kind: 'signal', severity: 'medium', original_severity: 'high', title: 'Harmless rename', explanation: 'A variable was renamed.', judgment: 'no_risk', rationale: 'Only the name changed.', status: 'OBSERVED', path: 'pay.go', line: 5, end_line: 5, side: 'new' }],
      dismissed: [{ id: 'signal:s2', kind: 'signal', severity: 'low', title: 'Only a comment', original_title: 'comment only', explanation: 'A comment changed.', judgment: 'no_risk', rationale: 'Line 4 is a comment.', status: 'OBSERVED', set_aside: true, path: 'pay.go', line: 4, end_line: 4, side: 'new' }],
    });
    renderReport();
    const reportHead = document.getElementById('report-head');
    assert(reportHead.querySelector('.reviewer-summary').textContent.includes('Main risk: refunds.') && reportHead.querySelectorAll('img').length === 0, 'reviewer summary rendered as text');
    assert(reportHead.textContent.includes('AI suspicions') && reportHead.textContent.includes('set aside by AI'), 'AI counters shown');
    const aiAlert = document.querySelector('#alerts .alert');
    assert(aiAlert.querySelector('.alert-title').textContent === 'Refunds are no longer checked' && aiAlert.textContent.includes('AI: risk'), 'plain title and judgment chip');
    const lowered = document.querySelectorAll('#alerts .alert')[1];
    assert(lowered.querySelector('.chip.lowered').textContent === 'was high' && lowered.textContent.includes('AI: no risk'), 'a lowered severity shows the linter one');
    state.feedback = { commit: state.commit, learning: true, entries: [] };
    aiAlert.querySelector('.alert-head').click();
    // Team feedback: a vote, a comment and a reply on the expanded finding.
    const feedbackBox = () => document.querySelector('#alerts .feedback');
    assert(feedbackBox() && feedbackBox().textContent.includes('Was this finding useful?'), 'an expanded finding takes feedback');
    feedbackBox().querySelector('[data-vote="up"]').click();
    await settle();
    const voteCall = fixtureCalls.find((call) => call.path.endsWith('/feedback') && call.init?.method === 'POST');
    assert(voteCall && JSON.parse(voteCall.init.body).alert_id === 'signal:s1' && JSON.parse(voteCall.init.body).vote === 'up' && voteCall.init.headers['X-Probe-CSRF'] === 'csrf', 'a vote posts the finding with CSRF');
    assert(feedbackBox().querySelector('[data-vote="up"]').getAttribute('aria-pressed') === 'true' && feedbackBox().querySelector('[data-vote="up"]').textContent.includes('· 1'), 'my vote is shown');
    feedbackBox().querySelector('textarea').value = 'The guard moved to the caller.';
    feedbackBox().querySelector('[data-feedback-send]').click();
    await settle();
    assert(feedbackBox().querySelectorAll('.feedback-comment').length === 1, 'the comment is listed');
    feedbackBox().querySelector('[data-reply]').click();
    await settle();
    assert(feedbackBox().querySelector('textarea').placeholder === 'Your reply', 'reply mode');
    feedbackBox().querySelector('textarea').value = 'Agreed.';
    feedbackBox().querySelector('[data-feedback-send]').click();
    await settle();
    const replyCall = fixtureCalls.filter((call) => call.path.endsWith('/feedback') && call.init?.method === 'POST').at(-1);
    assert(JSON.parse(replyCall.init.body).reply_to === 'fb-1', 'a reply names its comment');
    const comments = feedbackBox().querySelectorAll('.feedback-comment');
    assert(comments.length === 2 && comments[1].style.marginLeft === '16px' && comments[1].textContent.includes('Agreed.'), 'the reply is nested under its comment');
    assert(document.querySelector('#alerts .ai-reading').textContent.includes('The guard was removed.') && document.querySelector('#alerts .alert-body').textContent.includes('Linter: validation removed'), 'AI reading and linter title in the alert body');
    const dismissedBox = document.querySelector('#extras details.dismissed');
    assert(dismissedBox && !dismissedBox.open && dismissedBox.querySelector('summary').textContent.includes('(1)'), 'set-aside items folded with their count');
    assert(!document.getElementById('alerts').textContent.includes('Only a comment'), 'set-aside items are not alerts');
    dismissedBox.open = true;
    dismissedBox.dispatchEvent(new Event('toggle'));
    dismissedBox.querySelector('.alert-head').click();
    const reopened = document.querySelector('#extras details.dismissed');
    assert(reopened.open && reopened.textContent.includes('Why: Line 4 is a comment.'), 'a set-aside item unfolds and its section stays open');
    state.expanded.clear();
    state.showDismissed = false;
    state.view = plainView; renderReport();
    assert(!document.querySelector('#extras details.dismissed') && !document.querySelector('#report-head .reviewer-summary'), 'no AI sections without a reviewer');
    const severity = document.getElementById('severity');
    assert(document.querySelector('.topbar').contains(severity), 'severity threshold in the top bar');
    assert(severity.type === 'range', 'severity threshold uses a horizontal slider');
    const period = document.getElementById('period');
    const repoMeta = () => document.querySelector('.repo-meta').textContent;
    assert(period.type === 'range' && period.compareDocumentPosition(severity) & Node.DOCUMENT_POSITION_FOLLOWING, 'period slider before the review threshold');
    assert(document.getElementById('period-value').textContent === '1d', 'default period is one day');
    assert(repoMeta().includes('Human review required') && repoMeta().includes('1 commit · 1d'), 'worst status of the day: ' + repoMeta());
    period.value = '6'; period.dispatchEvent(new Event('input'));
    assert(repoMeta().includes('reproduced issue') && repoMeta().includes('2 commits · 7d'), 'worst status of the week: ' + repoMeta());
    assert(localStorage.getItem('probe.hub.period') === '6', 'period remembered');
    assert(document.getElementById('review-count').textContent.startsWith('0 repositories'), 'repository count follows the displayed blocked verdict');
    const recentCount = state.recent.get('repo').size;
    rememberRun('repo', { ...fixtureRun('plan'), commit: fixtureSHA('f'), queued_at: fixtureAgo(0) });
    assert(state.recent.get('repo').size === recentCount, 'plans never enter normal period aggregation');
    const selectedRepo = state.repos.get('repo');
    selectedRepo.recent_incomplete = true;
    renderRepos();
    assert(repoMeta().includes('partial history') && document.getElementById('review-count').textContent.includes('partial history'), 'truncation is visible in status and count');
    selectedRepo.recent_incomplete = false;

    period.value = '0'; period.dispatchEvent(new Event('input'));
    assert(repoMeta().includes('no commit in 1h'), 'empty period: ' + repoMeta());
    assert(document.getElementById('review-count').textContent.startsWith('0 repositories'), 'review count follows the period');
    period.value = '3'; period.dispatchEvent(new Event('input'));
    assert(document.getElementById('review-count').textContent === '1 repository · 1 commit to review', 'review count at low: ' + document.getElementById('review-count').textContent);
    severity.value = '3'; severity.dispatchEvent(new Event('input'));
    assert(document.querySelector('#report-head .verdict').textContent === 'Review below critical', 'high review not flagged at critical');
    assert(!document.getElementById('commit-tree').textContent.includes('Human review required'), 'tree badges follow the threshold');
    assert(document.getElementById('review-count').textContent === '0 repositories · 0 commits to review', 'review count follows the threshold');
    assert(localStorage.getItem('probe.hub.minSeverity') === '3', 'threshold remembered');
    assert(document.getElementById('severity-value').textContent === 'critical' && severity.getAttribute('aria-valuetext') === 'critical', 'visible and accessible slider value updated');
    severity.value = '2'; severity.dispatchEvent(new Event('input'));
    assert(document.querySelector('#report-head .verdict').textContent === 'Human review required', 'high review flagged at high');
    severity.value = '0'; severity.dispatchEvent(new Event('input'));
    fixtureReportError = true;
    await loadReport();
    assert(document.getElementById('report-empty').textContent.includes('cached result unavailable'), 'load errors stay visible');
    fixtureReportError = false;
    await loadReport();
    assert(document.querySelector('#report-head .verdict').textContent === 'Human review required' && document.getElementById('download').href.endsWith('/raw'), 'the report and its download come back');
    assert(fixtureCalls.filter((call) => call.path.endsWith('/analyze')).length === 1, 'loading cached results never launches an analysis');
    // An alert singles out only the lines it is about, and says why it singles out none.
    const diffFile = { path: 'hub/api.go', status: 'M', additions: 1, deletions: 0, hunks: [{ old_start: 1, old_lines: 1, new_start: 1, new_lines: 2, lines: [
      { kind: 'context', old_line: 1, new_line: 1, content: 'package hub' }, { kind: 'add', new_line: 2, content: 'if err != nil {}' }] }] };
    const wholeFile = { id: 'signal:path', kind: 'signal', severity: 'high', title: 'Lines added and removed in a sensitive file', path: 'hub/api.go', scope: 'file' };
    assert(alertLocation(wholeFile) === 'hub/api.go · whole file', 'file-level location: ' + alertLocation(wholeFile));
    const wholeDiff = renderDiff(diffFile, wholeFile);
    assert(!wholeDiff.querySelector('tr.focus') && wholeDiff.querySelector('.diff-note').textContent.includes('whole file'), 'a file-level alert highlights no line');
    const lineAlert = { id: 'signal:err', kind: 'signal', severity: 'medium', title: 'Error handling changed', path: 'hub/api.go', line: 2, end_line: 2, side: 'new' };
    const lineDiff = renderDiff(diffFile, lineAlert);
    assert(lineDiff.querySelectorAll('tr.focus').length === 1 && !lineDiff.querySelector('.diff-note'), 'a line alert highlights its line');
    const outside = renderDiff(diffFile, { ...lineAlert, line: 40, end_line: 40 });
    assert(!outside.querySelector('tr.focus') && outside.querySelector('.diff-note').textContent.startsWith('Line 40 is outside the recorded diff'), 'a line outside the diff is named');
    // Related file signals share a card, with all reasons and one diff per file.
    const savedView = state.view;
    const grouped = { id: 'group:cache-bust', kind: 'signal', severity: 'high', status: 'OBSERVED', title: 'Configuration value changed: KEY', members: [
      { ...wholeFile, id: 's1', detail: 'Configured sensitive path', reasons: ['sensitive_path'] },
      { ...wholeFile, id: 's2', title: 'Deployment configuration changed', detail: 'Inspect permissions', reasons: ['infrastructure_change'], explanation: 'A shared build default changed', judgment: 'uncertain' },
      { ...wholeFile, id: 's3', path: 'docker-compose.yml', detail: 'Compose pattern', reasons: ['sensitive_path'] },
      { ...wholeFile, id: 's4', path: 'devops/docker-compose.swarm.yml', detail: 'Swarm pattern', reasons: ['sensitive_path'] },
    ] };
    state.view = { alerts: [grouped], files: [diffFile, { ...diffFile, path: 'docker-compose.yml' }, { ...diffFile, path: 'devops/docker-compose.swarm.yml' }] };
    state.kind = 'all'; state.minSeverity = 0;
    renderKindFilter(); renderAlerts();
    assert(el('alert-count').textContent === '1 of 1 alerts shown' && el('kinds').textContent.includes('Signals (1)'), 'filters count groups');
    assert(el('alerts').querySelectorAll('.alert').length === 1 && el('alerts').textContent.includes('3 files · 4 signals grouped'), 'one card names the group size');
    el('alerts').querySelector('.alert-head').click();
    assert(el('alerts').querySelector('.alert-head').getAttribute('aria-expanded') === 'true', 'group expands accessibly');
    assert(el('alerts').querySelectorAll('.diff').length === 3 && !el('alerts').querySelector('tr.focus'), 'one complete diff per file with no invented line focus');
    for (const text of ['Configured sensitive path', 'Inspect permissions', 'sensitive_path', 'infrastructure_change', 'Compose pattern', 'Swarm pattern', 'A shared build default changed']) {
      assert(el('alerts').textContent.includes(text), 'group preserves ' + text);
    }
    state.minSeverity = 3; renderAlerts();
    assert(el('alert-count').textContent === '0 of 1 alerts shown', 'group obeys severity filter');
    renderKindFilter();
    assert(el('kinds').textContent.includes('Filtered (0)') && el('kinds').textContent.includes('Everything (1)'), 'filtered tab counts above the threshold');
    state.kind = 'everything'; renderAlerts();
    assert(el('alert-count').textContent === '1 of 1 alerts shown', 'everything ignores the severity filter');
    state.kind = 'all'; state.minSeverity = 0;
    state.view.files = [diffFile];
    assert(alertBody(grouped).textContent.includes('no diff for docker-compose.yml'), 'missing group diffs are explicit');
    const duplicate = { ...grouped, members: [grouped.members[0], { ...grouped.members[0], id: 'duplicate' }] };
    assert(alertBody(duplicate).querySelectorAll('.alert-detail').length === 1, 'identical explanations are displayed once');
    // With a PR summary, the list is laid out by its change areas, in its
    // order, each with the alerts it cites; what no area cites comes last.
    const areaChanges = [
      { area: 'Add agent sorting', summary: 'Agents sort by name.', refs: [{ path: 'hub/api.go', start_line: 2 }] },
      { area: 'Test agent sorting', summary: 'A test covers it.', refs: [] },
      { area: 'Nothing shown', summary: 'Docs.', refs: [] },
    ];
    state.view = { files: [diffFile], pr_summary: { changes: areaChanges, risks: [{ text: 'r', signal_ids: ['d'], hypothesis_ids: [], refs: [] }] }, alerts: [
      { ...lineAlert, id: 'signal:a', severity: 'high', area: 2, title: 'Type or safety checking suppression added' },
      { ...lineAlert, id: 'signal:b', area: 1, title: 'More branching constructs appear in the diff' },
      { ...lineAlert, id: 'focus:0', kind: 'focus', title: 'hub/api.go:2-2' },
      { ...lineAlert, id: 'signal:c', severity: 'low', area: 1, title: 'Possible public declaration added' },
      { ...lineAlert, id: 'signal:d', title: 'Cited by a risk only' },
    ] };
    renderAlerts();
    const areaHeads = [...el('alerts').querySelectorAll('.area')].map((h) => h.querySelector('b').textContent + ':' + h.querySelector('.note').textContent);
    assert(areaHeads.join('|') === 'Add agent sorting:2 alerts|Test agent sorting:1 alert|Nothing shown:no alert|Other alerts:1 alert the summary does not cite', 'areas in summary order: ' + areaHeads.join('|'));
    const order = [...el('alerts').querySelectorAll('.alert-title')].map((t) => t.textContent);
    assert(order.join('|') === 'More branching constructs appear in the diff|Possible public declaration added|Type or safety checking suppression added|hub/api.go:2-2', 'alerts follow their area: ' + order.join('|'));
    const areaLink = el('alerts').querySelector('.area .ref-link');
    areaLink.click();
    assert(el('alerts').querySelector('.area .summary-diff tr.focus'), 'an area unfolds the code it cites');
    state.minSeverity = 2; renderAlerts();
    assert(el('alerts').querySelector('.area .note').textContent === '0 of 2 alerts' && el('alerts').querySelectorAll('.area').length === 3, 'areas stay while the filter hides their alerts');
    state.minSeverity = 0;
    state.view.pr_summary = null;
    renderAlerts();
    assert(!el('alerts').querySelector('.area') && el('alerts').querySelectorAll('.alert').length === 5, 'a plain list without a summary');
    // The summary head links each statement to the code it cites: a link
    // unfolds that diff under it, and a cited alert the list shows opens there.
    state.view = { files: [diffFile], checks: [{ id: 'c1', kind: 'test', status: 'PASS' }], dismissed: [],
      alerts: [{ ...lineAlert, id: 'issue:h1#1', kind: 'issue', status: 'UNVERIFIED', area: 1 }, { ...lineAlert, id: 'signal:s9', title: 'Errors are logged', detail: 'The error is only logged.' }] };
    const prSummary = { title: 'Check errors', overview: 'Errors are checked.', model: 'm',
      changes: [{ area: 'API', summary: 'Adds a check.', refs: [{ path: 'hub/api.go', start_line: 2 }], signal_ids: [], hypothesis_ids: ['h1'] }],
      behavior_changes: [{ text: 'Errors stop the request', refs: [{ path: 'hub/api.go', start_line: 2 }] }],
      risks: [{ text: 'Unverified: errors are swallowed', severity: 'high', signal_ids: ['gone', 's9'], hypothesis_ids: ['h1'], refs: [] }],
      review_focus: [{ text: 'The new branch', severity: 'medium', refs: [{ path: 'hub/api.go', start_line: 1, end_line: 2, quote: 'if err != nil {}' }] }],
      testing: [], rejected_citations: 2 };
    state.view.pr_summary = prSummary;
    renderAlerts();
    const prBox = renderPRSummary(prSummary);
    document.body.appendChild(prBox);
    assert(!prBox.textContent.includes('Adds a check.'), 'change areas live in the list, not in the head');
    const refLinks = [...prBox.querySelectorAll('.ref-link')];
    assert(refLinks.map((l) => l.textContent).join('|') === 'api.go:2|Error handling changed|Errors are logged|✓ api.go:1-2' && refLinks[3].classList.contains('verified') && refLinks[3].title.includes('if err != nil {}'), 'summary links: ' + refLinks.map((l) => l.textContent).join('|'));
    refLinks[0].click();
    assert(prBox.querySelectorAll('.summary-diff tr.focus').length === 1 && refLinks[0].getAttribute('aria-expanded') === 'true', 'a code link unfolds its diff on the cited line');
    refLinks[3].click();
    assert(prBox.querySelectorAll('.summary-diff').length === 2 && prBox.querySelectorAll('.summary-diff tr.focus').length === 3, 'each statement unfolds its own diff');
    refLinks[0].click();
    assert(prBox.querySelectorAll('.summary-diff').length === 1 && refLinks[0].getAttribute('aria-expanded') === 'false', 'a second click folds it');
    refLinks[1].click();
    assert(refLinks[1].tagName === 'SPAN' && !state.expanded.has('issue:h1#1') && !prBox.querySelector('.summary-diff .alert-body'), 'an alert the list shows is only named, not a link to the list');
    assert(![...el('alerts').querySelectorAll('.alert-title')].some((t) => t.textContent === 'Errors are logged') && !el('alerts').querySelector('.area.other'), 'an alert a risk cites is not repeated under Other alerts');
    refLinks[2].click();
    assert(prBox.querySelector('.summary-diff .alert-body') && prBox.textContent.includes('The error is only logged.'), 'it unfolds whole under the risk');
    assert(prBox.textContent.includes('Probe executed 1 check for this review: 1 PASS.'), 'testing states what Probe executed');
    assert(prBox.textContent.includes('✓ marks a citation') && prBox.textContent.includes('2 citations quoted code that is not in the diff'), 'the caveat explains checked and dropped citations');
    const prMarkdown = prSummaryMarkdown(prSummary);
    assert(prMarkdown.includes('- **API**: Adds a check. — hub/api.go:2\n  - **UNVERIFIED / medium** Error handling changed — hub/api.go:2\n') && prMarkdown.includes('- **high** Unverified: errors are swallowed — Error handling changed — hub/api.go:2 (unverified issue), Errors are logged — hub/api.go:2 (signal)\n') && prMarkdown.includes('- **medium** The new branch — hub/api.go:1-2 ✓\n'), 'markdown cites code:\n' + prMarkdown);
    // A risk leads with the severity the AI estimated, in its color, and
    // follows the alert filters: the threshold hides it, Everything shows it.
    const highRisk = prBox.querySelector('#pr-risks li.rated');
    assert(highRisk.classList.contains('sev-high') && highRisk.querySelector('.chip').textContent === 'high', 'a risk shows its severity in its color');
    const shownFocus = () => [...prBox.querySelectorAll('#pr-focus li.rated')].map((li) => li.querySelector('.chip').textContent).join('|');
    assert(shownFocus() === 'medium' && prBox.querySelector('#pr-focus li.rated').classList.contains('sev-medium'), 'a place to look shows its severity: ' + shownFocus());
    prSummary.risks.push({ text: 'Minor naming', severity: 'low', signal_ids: [], hypothesis_ids: [], refs: [{ path: 'hub/api.go' }] });
    state.minSeverity = 2; renderAlerts();
    const shownRisks = () => [...prBox.querySelectorAll('#pr-risks li.rated')].map((li) => li.querySelector('.chip').textContent).join('|');
    assert(shownRisks() === 'high' && prBox.querySelector('#pr-risks .note').textContent.startsWith('1 risk is below the selected severity'), 'the threshold hides a lower risk: ' + shownRisks());
    assert(shownFocus() === '' && prBox.querySelector('#pr-focus .note').textContent.startsWith('1 place to look is below the selected severity'), 'the threshold hides a lower place to look: ' + shownFocus());
    state.kind = 'everything'; renderAlerts();
    assert(shownRisks() === 'high|low' && !prBox.querySelector('#pr-risks .note') && shownFocus() === 'medium', 'Everything shows every risk and place to look: ' + shownRisks());
    state.kind = 'all'; state.minSeverity = 3; renderAlerts();
    assert(shownRisks() === '' && prBox.querySelector('#pr-risks .note').textContent.startsWith('2 risks are'), 'every risk can be hidden');
    state.minSeverity = 0; renderAlerts();
    prBox.remove(); state.expanded.delete('issue:h1#1');
    state.view = savedView; state.minSeverity = 0; state.expanded.delete(grouped.id);
    // A partial SSE payload must not erase the known queue date or verdict.
    const liveCommit = fixtureSHA('f');
    const queued = fixtureAgo(0.1);
    rememberRun('repo', { commit: liveCommit, status: 'done', queued_at: queued, summary: { verdict: 'blocked' } });
    rememberRun('repo', { commit: liveCommit, queued_at: undefined });
    assert(state.recent.get('repo').get(liveCommit).queued_at === queued, 'undefined queue time preserves the known date');
    for (const missing of [null, '', 0, '0001-01-01T00:00:00Z', 'invalid']) {
      fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { commit: liveCommit, status: 'done', queued_at: missing, error: 'private error', message: 'private message', author: 'private author', intent: 'private intent' } }) });
      const remembered = state.recent.get('repo').get(liveCommit);
      assert(remembered.queued_at === queued && remembered.summary.verdict === 'blocked', 'SSE preserves date and verdict');
      assert(!('error' in remembered) && !('message' in remembered) && !('author' in remembered) && !('intent' in remembered), 'recent browser cache only keeps aggregation fields');
    }
    setPeriod(0);
    assert(repoMeta().includes('reproduced issue'), 'dated blocking SSE update stays visible');
    state.recent.get('repo').delete(liveCommit);
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { commit: liveCommit, status: 'done', queued_at: '0001-01-01T00:00:00Z', summary: { verdict: 'blocked' } } }) });
    assert(repoMeta().includes('reproduced issue') && repoMeta().includes('analysis date unknown'), 'undated blocker remains visible with an explicit warning');
    assert(!repoMeta().includes('no commit in'), 'undated result never becomes an empty period');
    state.recent.get('repo').delete(liveCommit);
    rememberRun('repo', { commit: liveCommit, status: 'running', queued_at: fixtureAgo(300) });
    assert(periodRuns(fixtureRepo).some((run) => run.commit === liveCommit), 'old pending work remains visible');
    rememberRun('repo', { commit: liveCommit, status: 'done', finished_at: fixtureAgo(0.1), summary: { verdict: 'blocked' } });
    renderRepos();
    assert(repoMeta().includes('reproduced issue'), 'recent finish of an old run stays in the period');

    // A reconnect/periodic refresh catches results whose SSE was missed.
    fixtureRepo.recent.push({ commit: fixtureSHA('g'), status: 'done', queued_at: fixtureAgo(0.1), summary: { verdict: 'review', counts: fixtureCounts } });
    // The periodic refresh also reloads the commit tree in place.
    fixtureNewCommits = [{ sha: fixtureSHA('9'), parents: [fixtureSHA('a')], branches: ['main'], message: 'Pushed after the tree loaded', author: 'Ada' }];
    const selectedBefore = state.commit;
    const commitsCalls = fixtureCalls.filter((call) => call.path.endsWith('/commits')).length;
    await refreshDashboard();
    assert(fixtureCalls.filter((call) => call.path.endsWith('/commits')).length === commitsCalls + 1, 'the minute tick fetches the commit tree again');
    assert(document.getElementById('commit-tree').textContent.includes('Pushed after the tree loaded') && state.commit === selectedBefore, 'a new commit appears without changing the selection');
    fixtureNewCommits = [];
    assert(repoMeta().includes('Human review required') && document.getElementById('review-count').textContent.startsWith('1 repository'), 'refresh reconciles missed events and review counts');
    assert(!state.repos.get('repo').recent, 'repository snapshot does not duplicate the recent cache');
    const realNow = Date.now;
    try {
      Date.now = () => realNow() + 2 * 3600 * 1000;
      renderRepos();
      assert(repoMeta().includes('no commit in 1h'), 'period expires as the clock advances');
      assert(document.getElementById('review-count').textContent.startsWith('0 repositories'), 'review count ages with the window');
    } finally { Date.now = realNow; }

    // A snapshot started before a live event cannot roll that event back.
    let releaseRepos;
    fixtureReposGate = new Promise((resolve) => { releaseRepos = resolve; });
    const loading = loadRepos();
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { commit: fixtureSHA('g'), status: 'done', summary: { verdict: 'blocked' } } }) });
    releaseRepos();
    await loading;
    fixtureReposGate = null;
    assert(repoMeta().includes('reproduced issue'), 'in-flight snapshot preserves the newer SSE verdict');
    assert(state.recent.get('repo').get(fixtureSHA('g')).queued_at, 'in-flight merge preserves queue time');

    // A finished analysis whose live events were lost must not stay queued:
    // the activity list closes it and the stored history replaces the badge.
    const lostCommit = fixtureSHA('b');
    const lostKey = pendingKey('repo', lostCommit, 'normal');
    const lostAt = new Date(Date.now() - 1000).toISOString();
    followQueued('repo', { commit: lostCommit, variant: 'normal', queued_at: lostAt });
    assert(displayedRun(lostCommit, 'normal')?.status === 'queued', 'enqueued attempt shown as queued');
    fixtureActivities = [
      { repo_key: 'repo', commit: lostCommit, variant: 'normal', status: 'done', queued_at: fixtureAgo(1), finished_at: fixtureAgo(1) },
      { repo_key: 'repo', commit: lostCommit, variant: 'normal', status: 'running', queued_at: lostAt },
    ];
    await syncPending();
    assert(state.pending.get(lostKey)?.status === 'running', 'an earlier finished attempt does not close the new one');
    fixtureActivities[1] = { ...fixtureActivities[1], status: 'done', finished_at: new Date().toISOString() };
    fixtureRuns = [...fixtureRuns, { commit: lostCommit, variant: 'normal', status: 'done', queued_at: lostAt, summary: { verdict: 'clear' } }];
    await syncPending();
    await settle(); await settle();
    assert(!state.pending.has(lostKey), 'polling closes an attempt whose events were lost');
    assert(displayedRun(lostCommit, 'normal')?.status === 'done', 'history replaces the queued badge');
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { commit: lostCommit, variant: 'normal', status: 'queued', queued_at: lostAt } }) });
    assert(!state.pending.has(lostKey), 'a late queued event for a finished attempt is ignored');
    followQueued('repo', { commit: lostCommit, variant: 'normal', queued_at: lostAt });
    assert(!state.pending.has(lostKey), 'a late enqueue response for a finished attempt is ignored');
    assert(state.recentLimit === 7, 'browser uses the server supplied history cap');
    for (let i = 0; i < 12; i++) {
      rememberRun('repo', { commit: `cap-${i}`, status: 'done', queued_at: fixtureAgo(0) });
    }
    assert(state.recent.get('repo').size === 7 && state.repos.get('repo').recent_incomplete, 'live history obeys the API cap and marks omissions');

    // The activity list cancels a queued attempt and runs a finished one again.
    const cancelCommit = fixtureSHA('f');
    fixtureActivities = [
      { repo_key: 'repo', commit: cancelCommit, variant: 'normal', status: 'queued', queued_at: fixtureAgo(0) },
      { repo_key: 'repo', commit: cancelCommit, variant: 'plan', status: 'done', queued_at: fixtureAgo(1), finished_at: fixtureAgo(1) },
      { repo_key: 'repo', commit: lostCommit, variant: 'normal', status: 'cancelled', queued_at: fixtureAgo(2), finished_at: fixtureAgo(2) },
    ];
    activityButton.click();
    await settle();
    const rowButtons = (index, label) => Array.from(document.querySelectorAll('#activity-list .activity-row')[index].querySelectorAll('button')).filter((b) => b.textContent === label);
    assert(rowButtons(0, 'Cancel').length === 1 && rowButtons(0, 'Run again').length === 0, 'a queued analysis can be cancelled');
    assert(rowButtons(1, 'Run again').length === 1 && rowButtons(1, 'Cancel').length === 0, 'a finished analysis can run again');
    assert(rowButtons(2, 'Run again').length === 0 && rowButtons(2, 'Cancel').length === 0 && activityText().includes('Cancelled'), 'a cancelled analysis offers no action');
    rowButtons(0, 'Cancel')[0].click();
    await settle();
    const cancelCall = fixtureCalls.find((call) => call.path === '/api/repos/repo/cancel');
    assert(cancelCall && cancelCall.init.method === 'POST' && JSON.parse(cancelCall.init.body).commit === cancelCommit && cancelCall.init.headers['X-Probe-CSRF'] === 'csrf', 'cancel posts the attempt with CSRF');
    rowButtons(1, 'Run again')[0].click();
    await settle();
    const rerunCall = fixtureCalls.find((call) => call.path === '/api/repos/repo/rerun');
    assert(rerunCall && JSON.parse(rerunCall.init.body).variant === 'plan', 'run again posts the variant');
    assert(state.pending.get(pendingKey('repo', cancelCommit, 'plan'))?.status === 'queued', 'the new attempt is followed');
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'run', repo_key: 'repo', run: { commit: cancelCommit, variant: 'plan', status: 'cancelled', queued_at: state.pending.get(pendingKey('repo', cancelCommit, 'plan')).queued_at } }) });
    assert(!state.pending.has(pendingKey('repo', cancelCommit, 'plan')), 'a cancelled event closes the pending attempt');
    closeModal();
    await settle();
    // Coding rules are edited from the repository list and saved with CSRF.
    const rulesButton = () => Array.from(document.querySelectorAll('.repo-actions button')).find((b) => (b.getAttribute('aria-label') || '').startsWith('Review settings'));
    assert(rulesButton() && rulesButton().textContent === '' && rulesButton().querySelector('svg') && !rulesButton().classList.contains('has-rules'), 'the review settings button is a gear icon without the rules mark');
    assert(!Array.from(document.querySelectorAll('.repo-actions button')).some((b) => b.textContent === 'Stop monitoring'), 'stop monitoring is not in the repository list');
    rulesButton().click();
    await settle();
    assert(!el('modal').classList.contains('hidden') && el('coding-rules').value === '', 'the rules dialog opens empty');
    assert(el('learning-enabled').checked && el('learned-summary').textContent.includes('signal:no_test_change: useful 0, not useful 3; code changed after it 1, left unchanged 4'), 'learning is on by default and shows what was learned');
    el('coding-rules').value = '- Never log credentials.';
    el('learning-enabled').checked = false;
    el('coding-rules-save').click();
    await settle();
    const rulesCall = fixtureCalls.find((call) => call.path === '/api/repos/repo/rules');
    assert(rulesCall && rulesCall.init.method === 'PUT' && JSON.parse(rulesCall.init.body).rules === '- Never log credentials.' && rulesCall.init.headers['X-Probe-CSRF'] === 'csrf', 'saving puts the rules with CSRF');
    assert(el('modal').classList.contains('hidden') && state.repos.get('repo').coding_rules === '- Never log credentials.', 'the saved rules update the repository');
    const learningCall = fixtureCalls.find((call) => call.path === '/api/repos/repo/learning' && call.init?.method === 'PUT');
    assert(learningCall && JSON.parse(learningCall.init.body).enabled === false && state.repos.get('repo').learning === false, 'switching learning off is saved');
    assert(rulesButton().classList.contains('has-rules'), 'a repository with rules shows it');
    rulesButton().click();
    await settle();
    assert(el('coding-rules').value === '- Never log credentials.', 'the dialog shows the saved rules');
    // The review history is a tab beside the coding rules.
    assert(el('tab-coding-rules').getAttribute('aria-selected') === 'true' && el('review-history-panel').hidden, 'the dialog opens on the coding rules');
    assert([...el('review-settings-panel').querySelectorAll('.settings-section .settings-title')].map((h) => h.textContent).slice(0, 2).join('|') === 'Coding rules|Learning from team feedback', 'each setting is its own section');
    const rulesCardHeight = document.querySelector('.modal-card').offsetHeight;
    el('tab-review-history').click();
    await settle();
    assert(document.querySelector('.modal-card').offsetHeight === rulesCardHeight, 'the history tab keeps the size of the coding rules');
    const historyRows = el('review-history-panel').querySelectorAll('.review-history li');
    assert(el('review-settings-panel').hidden && el('modal-footer').hidden && historyRows.length === 2, 'the history tab lists the review actions');
    assert(historyRows[0].textContent.includes('mark withdrawn') && historyRows[1].textContent.includes('reviewed') && historyRows[1].textContent.includes('Merge feature') && historyRows[1].textContent.includes('octocat'), 'each action shows what, which commit and who');
    el('tab-coding-rules').click();
    assert(!el('review-settings-panel').hidden && !el('modal-footer').hidden, 'back to the coding rules');
    closeModal();
    assert(document.querySelector('.modal-card').style.height === '', 'closing the dialog frees the card size for the next one');
    await settle();
    assert(!document.body.dataset.testResult, document.body.dataset.testResult);
    document.body.dataset.testResult = 'PASS';
  } catch (err) { fixtureFail(err); }
});
