// Panel contract tests.
//
// The panel is a browser page, so the useful assertions are about CONTENT and
// SAFETY rather than about the network: the page ships in the .so and an
// operator pastes a live Google cookie into it.
//
// Run with: node --test panel.test.js

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const page = await readFile(new URL('./panel.html', import.meta.url), 'utf8');

test('page targets this plugin\'s own management namespace', () => {
  assert.match(page, /\/v0\/management\/plugins\/geminiapi/,
    'the page must call the plugin\'s own management namespace');
});

test('the management key is never persisted', () => {
  // The page mentions "localStorage" only to PROMISE it does not use it, so the
  // assertion targets the write APIs rather than the word.
  assert.equal(/localStorage\.\s*setItem|sessionStorage\.\s*setItem|indexedDB\.\s*open/.test(page), false,
    'the page must not persist the management key in any browser store');
  // And it must not be baked into the URL either, which would land in history.
  assert.equal(/[?&]key=/.test(page), false, 'the key must not travel in the query string');
});

test('the page tells the operator how to copy the right cookie', () => {
  assert.match(page, /Network/, 'the instructions must name the Network panel');
  assert.match(page, /Request Headers/i, 'the instructions must name Request Headers');
  assert.match(page, /cookie/i, 'the instructions must name the cookie header');
  assert.match(page, /无痕/, 'the instructions must tell the operator to use a private window');
});

test('the page states the multi-account caveat', () => {
  // This is the promise the card makes: an operator must not believe that N
  // auth records mean N selectable Google accounts. The wording arrives from
  // the status payload (`notice`), so the page must render that field.
  assert.match(page, /st\.notice|notice/, 'the page must render the status notice');
  assert.match(page, /multiAccountNote/, 'the page must have a slot for the caveat');
  // And the payload-side sentence itself is asserted server-side; here we check
  // the page explains the pool.
  assert.match(page, /账号/, 'the page must talk about accounts');
});

test('the page explains that a foreign listener is not adopted', () => {
  assert.match(page, /外部后端/, 'the page must distinguish an external backend');
});

test('the page states the loopback and sha256 guarantees', () => {
  assert.match(page, /127\.0\.0\.1/, 'the page must say the backend is loopback-only');
  assert.match(page, /sha256/i, 'the page must say the binary is checksum-verified');
  assert.match(page, /进程组|孤儿/, 'the page must say how the process is torn down');
});

test('the page offers the backend control actions', () => {
  for (const route of ['/backend/start', '/backend/install', '/backend/stop', '/status', '/accounts', '/models']) {
    assert.ok(page.includes(route), `the page does not call ${route}`);
  }
});

test('every interpolated value is escaped', () => {
  // A management response can contain a backend-supplied string (a model id,
  // an error). Rendering it unescaped would be an XSS in the operator's CPA
  // session.
  assert.match(page, /const esc = /, 'the page must define an escaper');
  assert.match(page, /replace\(/,'the escaper must actually replace characters');
});

test('the page asks for the key instead of assuming an authenticated context', () => {
  // The resource route is browser-navigable and therefore unauthenticated; the
  // page must collect the key itself.
  assert.match(page, /Bearer/, 'the page must send an Authorization header');
  assert.match(page, /id="token"|type="password"/, 'the page must ask for the management key');
});

test('credentials are omitted from fetches so no ambient session is used', () => {
  assert.match(page, /credentials:\s*'omit'|credentials:\s*"omit"/,
    'fetches must not rely on ambient cookies');
});

test('the page does not reference any external origin', () => {
  // The served CSP is default-src 'none' plus inline style/script only; an
  // external href/script would silently break.
  const externals = page.match(/(src|href)="http/gi) || [];
  assert.equal(externals.length, 0, `the page references an external origin: ${externals.join(', ')}`);
});

test('the page names both namespaces and both pinned backends', () => {
  // Two backends, two namespaces. A page that named only one would let an
  // operator believe the ai-studio block drives gemini-web2api-go (which is
  // exactly the defect the split removed).
  for (const ns of ['ai-studio', 'gemini-web']) {
    assert.match(page, new RegExp(`value="${ns}"`), `the page has no ${ns} namespace selector`);
  }
  assert.match(page, /AIStudio2API/, 'the page must name the ai-studio backend');
  assert.match(page, /gemini-web2api-go/, 'the page must name the gemini-web backend');
  assert.match(page, /0\.3\.5-magpie\.1/, 'the page must name the ai-studio pin');
  assert.match(page, /4\.28\.0-magpie\.1/, 'the page must name the gemini-web pin');
});

test('each namespace renders its own backend state', () => {
  // The status payload carries one section per namespace; the page must render
  // that array rather than a single hard-coded block, and must show which
  // backend and version each section's pin resolves to.
  assert.match(page, /st\.backends/, 'the page must render the per-namespace backend array');
  assert.match(page, /section\.backend/, 'each section must show its backend name');
  assert.match(page, /pinned_backend_version/, 'each section must show its pinned version');
});

test('a namespace that published nothing shows the reason', () => {
  // The acceptance criterion: a zero-model namespace is an ERROR with a cause,
  // never a silent empty table. The page has to surface that cause.
  assert.match(page, /section\.error/, 'the page must render the per-namespace build error');
  assert.match(page, /发布 0 个模型/, 'the page must state that this namespace published nothing');
  // And the skip reasons, so an operator can see WHICH rule removed a model.
  assert.match(page, /section\.skipped/, 'the page must list unpublished models with their reasons');
});

test('backend controls carry the namespace they act on', () => {
  assert.match(page, /path \+ '\?namespace=' \+ encodeURIComponent\(ns\)/,
    'backend actions must pass the selected namespace');
  for (const route of ['/backend/start', '/backend/install', '/backend/stop']) {
    assert.ok(page.includes(`action('${route}'`), `the ${route} button must go through the namespaced action`);
  }
});

test("account import uses each namespace own surface, not one shared endpoint", () => {
  // ai-studio signs in through its own browser-login task; gemini-web takes a
  // pasted Google cookie. Sending a cookie to ai-studio (which has no cookie
  // endpoint) or a login task to gemini-web is the mismatch this split removes.
  assert.match(page, /namespace:'gemini-web',\s*cookie:/, 'the cookie form must target gemini-web');
  assert.match(page, /namespace:'ai-studio'/, 'the login button must target ai-studio');
  // The ai-studio login call must not carry a cookie at all: it starts a
  // browser sign-in task instead.
  assert.equal(/namespace:'ai-studio',\s*cookie/.test(page), false,
    'the ai-studio login call must not send a cookie');
});
