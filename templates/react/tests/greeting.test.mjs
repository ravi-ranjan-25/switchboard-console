import test from 'node:test';
import assert from 'node:assert/strict';
import { greeting } from '../src/greeting.js';

test('greeting names the app', () => {
  assert.equal(greeting('Demo'), 'Welcome to Demo.');
});
