import test from 'node:test';
import assert from 'node:assert/strict';
import { healthPayload } from '../lib/health.mjs';

test('health payload reports ok and the app name', () => {
  const p = healthPayload(new Date('2026-01-01T00:00:00Z'));
  assert.equal(p.status, 'ok');
  assert.equal(p.app, '[[.Name]]');
  assert.equal(p.time, '2026-01-01T00:00:00.000Z');
});
