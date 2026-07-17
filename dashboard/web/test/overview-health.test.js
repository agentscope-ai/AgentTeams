import test from 'node:test';
import assert from 'node:assert/strict';
import { freshnessFromAge, healthDotClass, formatAge, parseRFC3339 } from '../src/panels/overview-health.js';

const NOW = Date.parse('2026-07-17T12:00:00.000Z');

test('parseRFC3339 returns null for empty or invalid input', () => {
  assert.equal(parseRFC3339(''), null);
  assert.equal(parseRFC3339(undefined), null);
  assert.equal(parseRFC3339('not-a-date'), null);
});

test('freshnessFromAge marks recent timestamps healthy', () => {
  const ts = new Date(NOW - 5 * 60 * 1000).toISOString();
  const health = freshnessFromAge(ts, NOW);
  assert.equal(health.label, 'healthy');
  assert.equal(health.cls, 'badge-ready');
  assert.match(health.age, /5m ago/);
});

test('freshnessFromAge marks 15m old timestamps degraded', () => {
  const ts = new Date(NOW - 15 * 60 * 1000).toISOString();
  const health = freshnessFromAge(ts, NOW);
  assert.equal(health.label, 'degraded');
  assert.equal(health.cls, 'badge-degraded');
});

test('freshnessFromAge marks 45m old timestamps down', () => {
  const ts = new Date(NOW - 45 * 60 * 1000).toISOString();
  const health = freshnessFromAge(ts, NOW);
  assert.equal(health.label, 'down');
  assert.equal(health.cls, 'badge-failed');
});

test('freshnessFromAge returns unknown when timestamp missing', () => {
  const health = freshnessFromAge(null, NOW);
  assert.equal(health.label, 'unknown');
  assert.equal(health.cls, 'badge-unknown');
  assert.equal(health.age, null);
});

test('formatAge renders seconds, minutes, hours, and days', () => {
  assert.equal(formatAge(NOW - 30 * 1000, NOW), '30s ago');
  assert.equal(formatAge(NOW - 2 * 60 * 1000, NOW), '2m ago');
  assert.equal(formatAge(NOW - 3 * 60 * 60 * 1000, NOW), '3h ago');
  assert.equal(formatAge(NOW - 2 * 24 * 60 * 60 * 1000, NOW), '2d ago');
});

test('healthDotClass maps probe status to CSS classes', () => {
  assert.equal(healthDotClass('healthy'), 'health-dot-healthy');
  assert.equal(healthDotClass('degraded'), 'health-dot-degraded');
  assert.equal(healthDotClass('down'), 'health-dot-down');
  assert.equal(healthDotClass('unknown'), 'health-dot-unknown');
  assert.equal(healthDotClass(undefined), 'health-dot-unknown');
});
