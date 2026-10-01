// CLI login page: the user signs in, enters (or follows a link with) the code
// `probe login` printed, sees which machine asks for access, and approves or
// denies it. The CLI polls the hub meanwhile and receives its token.
'use strict';

const LABELS = { github: 'Continue with GitHub', gitlab: 'Continue with GitLab' };
const el = (id) => document.getElementById(id);
let csrf = '';

function show(id) { el(id).classList.remove('hidden'); }
function hide(id) { el(id).classList.add('hidden'); }

function fail(message) {
  const box = el('error');
  box.textContent = message;
  box.classList.remove('hidden');
}

async function call(path, options = {}) {
  const init = { credentials: 'same-origin', method: options.method || 'GET', headers: { 'Accept': 'application/json' } };
  if (options.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.headers['X-Probe-CSRF'] = csrf;
    init.body = JSON.stringify(options.body);
  }
  const response = await fetch(path, init);
  const text = await response.text();
  const payload = text ? JSON.parse(text) : {};
  if (!response.ok) throw new Error(payload.error || ('request failed with ' + response.status));
  return payload;
}

async function lookup(code) {
  hide('error');
  try {
    const grant = await call('/api/device?code=' + encodeURIComponent(code));
    el('client').textContent = grant.client;
    el('shown-code').textContent = grant.user_code;
    el('expires').textContent = new Date(grant.expires_at).toLocaleTimeString();
    hide('code-form');
    show('confirm');
    el('approve').focus();
    return grant.user_code;
  } catch (err) {
    fail(err.message);
    return '';
  }
}

async function decide(code, approve) {
  el('approve').disabled = true;
  el('deny').disabled = true;
  try {
    await call('/api/device/decide', { method: 'POST', body: { user_code: code, approve } });
    hide('confirm');
    el('done').textContent = approve
      ? 'Approved. Return to your terminal: probe login finishes on its own.'
      : 'Denied. The CLI was not given access.';
    show('done');
  } catch (err) {
    fail(err.message);
    el('approve').disabled = false;
    el('deny').disabled = false;
  }
}

document.addEventListener('DOMContentLoaded', async () => {
  const initial = new URLSearchParams(window.location.search).get('code') || '';
  let me;
  try {
    me = await call('/api/me');
  } catch (err) {
    fail('The service is unreachable. Try again in a moment.');
    return;
  }
  if (!me.authenticated) {
    const holder = el('providers');
    const forges = (me.forges || []).slice().sort();
    if (!forges.length) {
      holder.textContent = 'No forge is configured on this deployment.';
    }
    const next = '/device.html' + (initial ? '?code=' + encodeURIComponent(initial) : '');
    for (const kind of forges) {
      const link = document.createElement('a');
      link.href = '/auth/' + encodeURIComponent(kind) + '/start?next=' + encodeURIComponent(next);
      link.textContent = LABELS[kind] || 'Continue with ' + kind;
      link.rel = 'nofollow';
      holder.appendChild(link);
    }
    show('providers');
    return;
  }
  csrf = me.csrf;
  el('login').textContent = me.user.login;
  let code = '';
  el('approve').addEventListener('click', () => decide(code, true));
  el('deny').addEventListener('click', () => decide(code, false));
  el('code-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    code = await lookup(el('code').value.trim());
  });
  el('code').value = initial;
  show('code-form');
  // A code from the link is still shown for confirmation, never approved
  // without a click.
  if (initial) {
    code = await lookup(initial);
  } else {
    el('code').focus();
  }
});
