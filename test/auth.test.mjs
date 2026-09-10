import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createAuthMiddleware } from '../agent/auth.js';

function responseRecorder() {
  return {
    statusCode: 200,
    headers: {},
    body: undefined,
    setHeader(name, value) { this.headers[name] = value; },
    status(code) { this.statusCode = code; return this; },
    send(body) { this.body = body; return this; },
    json(body) { this.body = body; return this; },
  };
}

function run(middleware, { path = '/api/orders', authorization, originalUrl = path } = {}) {
  const req = { path, originalUrl, headers: authorization ? { authorization } : {} };
  const res = responseRecorder();
  let nextCalled = false;
  middleware(req, res, () => { nextCalled = true; });
  return { res, nextCalled };
}

test('localhost Host and spoofed forwarding headers cannot bypass auth', () => {
  const middleware = createAuthMiddleware({ bearerToken: 'good-token', basicUser: 'alice', basicPass: 'correct' });
  const result = run(middleware, {
    authorization: undefined,
    originalUrl: '/api/orders',
  });
  assert.equal(result.res.statusCode, 401);
  assert.equal(result.nextCalled, false);
});

test('configured Bearer and Basic credentials are both accepted', () => {
  const middleware = createAuthMiddleware({ bearerToken: 'good-token', basicUser: 'alice', basicPass: 'correct' });
  assert.equal(run(middleware, { authorization: 'Bearer good-token' }).nextCalled, true);
  assert.equal(run(middleware, { authorization: `Basic ${Buffer.from('alice:correct').toString('base64')}` }).nextCalled, true);
});

test('health is the only unauthenticated exception', () => {
  const middleware = createAuthMiddleware({ bearerToken: 'good-token' });
  const health = run(middleware, { path: '/api/health', originalUrl: '/api/health' });
  assert.equal(health.nextCalled, true);
  const rootHealth = run(middleware, { path: '/health', originalUrl: '/health' });
  assert.equal(rootHealth.nextCalled, true);
  const dashboard = run(middleware, { path: '/api/health/orders', originalUrl: '/api/health/orders' });
  assert.equal(dashboard.res.statusCode, 401);
});

test('missing credentials fail closed', () => {
  const middleware = createAuthMiddleware({ bearerToken: '', basicUser: '', basicPass: '' });
  const result = run(middleware);
  assert.equal(result.res.statusCode, 401);
  assert.equal(result.nextCalled, false);
});
