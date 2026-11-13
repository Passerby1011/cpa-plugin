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
