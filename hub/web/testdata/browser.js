'use strict';

const fixtureSHA = (letter) => letter.repeat(40);
const fixtureRepo = { key: 'repo', full_name: 'acme/shop', provider: 'github', default_branch: 'main', web_url: 'https://github.com/acme/shop', has_policy: true, admin: true };
const fixtureCounts = { total: 1, high: 1, critical: 0, medium: 0, low: 0 };
const fixtureRun = (variant) => ({ commit: fixtureSHA('a'), variant, status: 'done', mode: 'lint', summary: { verdict: 'review', counts: fixtureCounts, reproduced: 0, unverified: 1, focused_lines: 2, changed_lines: 5, changed_files: 1, additions: 4, deletions: 1 } });
const fixtureAgo = (hours) => new Date(Date.now() - hours * 3600 * 1000).toISOString();
fixtureRepo.recent = [
  Object.assign(fixtureRun('normal'), { queued_at: fixtureAgo(2) }),
  { commit: fixtureSHA('e'), variant: 'normal', status: 'done', queued_at: fixtureAgo(80), summary: { verdict: 'blocked', counts: { total: 2, critical: 1, high: 1, medium: 0, low: 0 } } },
];
const fixtureCalls = [];
let fixtureStream;
let fixtureRuns = [fixtureRun('normal'), fixtureRun('plan')];
window.EventSource = class { constructor() { fixtureStream = this; } };
window.fetch = async (path, init) => {
  fixtureCalls.push({ path, init });
  let data;
  if (path === '/api/me') data = { authenticated: true, csrf: 'csrf', user: { login: 'octocat', provider: 'github' } };
  else if (path === '/api/repos') data = { repos: [fixtureRepo] };
  else if (path.endsWith('/commits')) data = { limited: false, branches: [{name:'main',sha:fixtureSHA('a')},{name:'feature/ui',sha:fixtureSHA('c')}], commits: [
    { sha: fixtureSHA('a'), parents: [fixtureSHA('b'), fixtureSHA('c')], branches: ['main'], message: 'Merge feature', author: 'Ada' },
    { sha: fixtureSHA('c'), parents: [fixtureSHA('d')], branches: ['feature/ui'], message: '<img src=x onerror=alert(1)>', author: 'Grace' },
    { sha: fixtureSHA('b'), parents: [fixtureSHA('d')], branches: [], message: 'Main branch work', author: 'Ada' },
    { sha: fixtureSHA('d'), parents: [], branches: [], message: 'Initial commit', author: 'Ada' },
  ] };
  else if (path.includes('/runs')) data = { runs: fixtureRuns };
  else if (path.includes('/reports/')) {
    const variant = new URL(path, location.origin).searchParams.get('variant');
    const run = fixtureRun(variant);
    data = { run, view: { summary: run.summary, alerts: [], files: [], unverified: ['No checks ran'], plan_drift: variant === 'plan' ? { status: 'conforming', decision: 'human_review_required', decision_reasons: ['No checks ran'] } : null }, plan: variant === 'plan' ? { proposal: { summary: 'Generated plan' } } : null };
  } else if (path.endsWith('/analyze')) data = { status: 'queued' };
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
    const repoPanel = document.querySelector('.repo-panel');
    assert(getComputedStyle(repoPanel).position === 'sticky', 'repository panel does not scroll with the page');
    assert(getComputedStyle(document.getElementById('repos')).overflowY === 'auto', 'repository list scrolls on its own');
    assert(parseFloat(getComputedStyle(repoPanel).top) >= document.querySelector('.topbar').getBoundingClientRect().height, 'repository panel stays below the top bar: ' + document.querySelector('.topbar').getBoundingClientRect().height);
    document.querySelector('.repo-name').click();
    await settle();
    assert(document.querySelectorAll('.commit-row').length === 4, 'all graph commits rendered');
    assert(document.querySelectorAll('.graph-node').length === 4, 'graph nodes rendered');
    assert(document.getElementById('commit-tree').textContent.includes('feature/ui'), 'branch tip visible');
    assert(document.querySelectorAll('#commit-tree img').length === 0, 'commit content must be text');
    assert(document.querySelectorAll('#commit-tree .chip.unknown').length === 6, 'gray unknown badge for uncached variants');
    assert(document.getElementById('commit-tree').textContent.includes('Human review required'), 'cached verdict badge');
    assert(!document.getElementById('commit-tree').textContent.includes('Normal'), 'the analysis badge has no mode prefix');
    assert(document.getElementById('commit-tree').textContent.includes('Plan: '), 'the plan badge keeps its prefix');
    assert(document.querySelector('#commit-tree .chip.warn').classList.contains('tone-high'), 'review badge tinted by the most severe alert');
    assert(document.querySelectorAll('.commit-row:first-child .commit-meta .chip').length === 1, 'equal plan and analysis reviews share one tree badge');
    // Exercise both severity orders, ties, missing alerts and every threshold.
    const reviewRun = (variant, level) => ({ ...fixtureRun(variant), summary: { verdict: 'review', counts: level ? { [level]: 1 } : {} } });
    for (const normalLevel of [...LEVELS, null]) {
      for (const planLevel of [...LEVELS, null]) {
        for (let threshold = 0; threshold < LEVELS.length; threshold++) {
          state.minSeverity = threshold;
          const badges = commitVerdictChips(reviewRun('normal', normalLevel), reviewRun('plan', planLevel));
          const highest = Math.max(LEVELS.indexOf(normalLevel || 'medium'), LEVELS.indexOf(planLevel || 'medium'));
          assert(badges.length === 1, 'reviews aggregate into one badge');
          const badge = badges[0];
          if (highest < threshold) {
            assert(badge.classList.contains('below-threshold'), 'aggregated review below threshold');
          } else {
            assert(badge.textContent === 'Human review required' && badge.classList.contains('tone-' + LEVELS[highest]), 'aggregate uses the higher severity');
          }
          assert(badge.title.includes('Analysis: ' + (normalLevel || 'medium')) && badge.title.includes('Plan: ' + (planLevel || 'medium')), 'hover retains both review severities');
        }
      }
    }
    state.minSeverity = 0;
    for (const other of [undefined, { status: 'queued' }, { status: 'running' }, { status: 'failed' }, { status: 'done', summary: { verdict: 'blocked' } }, { status: 'done', summary: { verdict: 'clear' } }]) {
      for (const runs of [[other, fixtureRun('plan')], [fixtureRun('normal'), other]]) {
        assert(commitVerdictChips(...runs).length === 2, 'non-review statuses retain separate badges');
      }
    }
    assert(fixtureCalls.every((call) => !call.path.endsWith('/analyze')), 'browsing must not run analyses');
    const firstCommit = document.querySelector('.commit-open');
    assert(!firstCommit.textContent.includes('aaaaaaaa') && firstCommit.title.includes(fixtureSHA('a')), 'commit id only on hover');
    assert(!document.getElementById('commit-tree').textContent.includes('Parents'), 'parents are drawn, not listed');
    assert(document.querySelector('.commit-row .chip.branch').textContent === 'main', 'branch name in the tree');
    const repoHead = document.querySelector('.repo-head');
    assert(repoHead.textContent.includes('Analyze now') && repoHead.textContent.includes('Activate monitoring'), 'repository actions next to the name');
    assert(!document.getElementById('repos').textContent.includes('.swiftproof.json'), 'no policy tag');
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
    assert(localStorage.getItem('swiftproof.commitColumnWidth') === splitter.getAttribute('aria-valuenow'), 'splitter width remembered');
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
    for (const card of document.querySelectorAll('.comparison-card')) {
      card.querySelector('button').click();
      await settle();
    }
    const requests = fixtureCalls.filter((call) => call.path.endsWith('/analyze'));
    assert(requests.length === 2, 'both analyses can be launched');
    for (let i = 0; i < 2; i++) {
      const body = JSON.parse(requests[i].init.body);
      assert(body.commit === fixtureSHA('c') && body.variant === (i ? 'plan' : 'normal'), 'exact selected commit and variant');
      assert(requests[i].init.headers['X-SwiftProof-CSRF'] === 'csrf', 'analysis includes CSRF');
    }
    fixtureStream.onmessage({ data: JSON.stringify({ type: 'report', repo_key: 'repo', commit: fixtureSHA('a'), run: fixtureRun('normal') }) });
    await settle();
    assert(document.querySelectorAll('.commit-row:first-child .commit-meta .chip').length === 1, 'live results keep reviews aggregated');
    assert(document.getElementById('report-head').textContent.includes('cccccccc'), 'live results do not steal selection');
    document.querySelector('.commit-open').click();
    await settle();
    const cards = document.querySelectorAll('.comparison-card');
    assert(cards.length === 2 && cards[1].contains(document.getElementById('plan-intent')), 'both mode cards, intent in the Plan card');
    assert(cards[0].querySelector('.card-head .chip') && cards[1].querySelector('.card-head .chip'), 'verdict on the title line');
    assert(cards[1].querySelector('.card-head label[for="plan-intent"]').textContent === 'Describe the task to see what impacts where planned', 'intent prompt beside the Plan title');
    assert(!document.getElementById('filters').classList.contains('hidden'), 'cached report shown on commit click');
    const commitLine = document.querySelector('#report-head .report-commit-line');
    assert(commitLine.querySelector('#selected-commit') && commitLine.querySelector('a').textContent === 'Open the commit', 'Open the commit beside the commit title');
    assert(!document.getElementById('report-head').textContent.includes('never approves'), 'no disclaimer line');
    assert(document.querySelector('#report-head .verdict').textContent === 'Human review required', 'report verdict rendered');
    assert(document.querySelector('#report-head .verdict').classList.contains('tone-high'), 'report verdict tinted by the most severe alert');
    assert(cards[0].querySelector('h3').textContent === 'Analysis', 'analysis card title');
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
    assert(localStorage.getItem('swiftproof.hub.period') === '6', 'period remembered');
    period.value = '0'; period.dispatchEvent(new Event('input'));
    assert(repoMeta().includes('no commit in 1h'), 'empty period: ' + repoMeta());
    assert(document.getElementById('review-count').textContent.startsWith('0 repositories'), 'review count follows the period');
    period.value = '3'; period.dispatchEvent(new Event('input'));
    assert(document.getElementById('review-count').textContent === '1 repository · 1 commit to review', 'review count at low: ' + document.getElementById('review-count').textContent);
    severity.value = '3'; severity.dispatchEvent(new Event('input'));
    assert(document.querySelector('#report-head .verdict').textContent === 'Review below critical', 'high review not flagged at critical');
    assert(!document.getElementById('commit-tree').textContent.includes('Human review required'), 'tree badges follow the threshold');
    assert(document.getElementById('review-count').textContent === '0 repositories · 0 commits to review', 'review count follows the threshold');
    assert(localStorage.getItem('swiftproof.hub.minSeverity') === '3', 'threshold remembered');
    assert(document.getElementById('severity-value').textContent === 'critical' && severity.getAttribute('aria-valuetext') === 'critical', 'visible and accessible slider value updated');
    severity.value = '2'; severity.dispatchEvent(new Event('input'));
    assert(document.querySelector('#report-head .verdict').textContent === 'Human review required', 'high review flagged at high');
    severity.value = '0'; severity.dispatchEvent(new Event('input'));
    cards[1].querySelectorAll('button')[1].click();
    await settle();
    assert(document.querySelector('#plan-result a').href.endsWith('?variant=plan'), 'download selected variant');
    assert(document.getElementById('plan-result').textContent.includes('Generated plan'), 'stored plan visible');
    // An alert singles out only the lines it is about, and says why it singles out none.
    const diffFile = { path: 'hub/api.go', status: 'M', additions: 1, deletions: 0, hunks: [{ old_start: 1, old_lines: 1, new_start: 1, new_lines: 2, lines: [
      { kind: 'context', old_line: 1, new_line: 1, content: 'package hub' }, { kind: 'add', new_line: 2, content: 'if err != nil {}' }] }] };
    const wholeFile = { id: 'signal:path', kind: 'signal', severity: 'high', title: 'Configured sensitive path changed', path: 'hub/api.go', scope: 'file' };
    assert(alertLocation(wholeFile) === 'hub/api.go · whole file', 'file-level location: ' + alertLocation(wholeFile));
    const wholeDiff = renderDiff(diffFile, wholeFile);
    assert(!wholeDiff.querySelector('tr.focus') && wholeDiff.querySelector('.diff-note').textContent.includes('whole file'), 'a file-level alert highlights no line');
    const lineAlert = { id: 'signal:err', kind: 'signal', severity: 'medium', title: 'Error handling changed', path: 'hub/api.go', line: 2, end_line: 2, side: 'new' };
    const lineDiff = renderDiff(diffFile, lineAlert);
    assert(lineDiff.querySelectorAll('tr.focus').length === 1 && !lineDiff.querySelector('.diff-note'), 'a line alert highlights its line');
    const outside = renderDiff(diffFile, { ...lineAlert, line: 40, end_line: 40 });
    assert(!outside.querySelector('tr.focus') && outside.querySelector('.diff-note').textContent.startsWith('Line 40 is outside the recorded diff'), 'a line outside the diff is named');
    assert(!document.body.dataset.testResult, document.body.dataset.testResult);
    document.body.dataset.testResult = 'PASS';
  } catch (err) { fixtureFail(err); }
});
