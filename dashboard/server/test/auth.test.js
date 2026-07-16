'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { createBasicAuth, REALM } = require('../src/auth');

function basicHeader(user, pass) {
  return { authorization: 'Basic ' + Buffer.from(`${user}:${pass}`, 'utf8').toString('base64') };
}

test('createBasicAuth: when auth is disabled, every request is allowed', () => {
  const check = createBasicAuth({ enabled: false, username: '', password: '' });
  assert.equal(check({ headers: {} }).ok, true);
  assert.equal(check({ headers: { authorization: 'Basic garbage' } }).ok, true);
  assert.equal(check({}).ok, true);
});

test('createBasicAuth: a valid Basic header authenticates the request', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 's3cret' });
  const req = { headers: basicHeader('admin', 's3cret') };
  assert.equal(check(req).ok, true);
});

test('createBasicAuth: a wrong password is rejected', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 's3cret' });
  const req = { headers: basicHeader('admin', 'wrong') };
  assert.equal(check(req).ok, false);
});

test('createBasicAuth: a wrong username is rejected', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 's3cret' });
  const req = { headers: basicHeader('root', 's3cret') };
  assert.equal(check(req).ok, false);
});

test('createBasicAuth: a missing Authorization header is rejected', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 's3cret' });
  assert.equal(check({ headers: {} }).ok, false);
  assert.equal(check({}).ok, false);
});

test('createBasicAuth: a malformed Authorization header is rejected', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 's3cret' });
  assert.equal(check({ headers: { authorization: 'Bearer xyz' } }).ok, false);
  assert.equal(check({ headers: { authorization: 'Basic !!not-base64!!' } }).ok, false);
  assert.equal(check({ headers: { authorization: 'Basic' } }).ok, false);
});

test('createBasicAuth: a credential string with no colon is rejected', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 's3cret' });
  const header = { authorization: 'Basic ' + Buffer.from('nocolonhere', 'utf8').toString('base64') };
  assert.equal(check({ headers: header }).ok, false);
});

test('createBasicAuth: a password containing a colon authenticates correctly', () => {
  const check = createBasicAuth({ enabled: true, username: 'admin', password: 'p:a:s:s' });
  const req = { headers: basicHeader('admin', 'p:a:s:s') };
  assert.equal(check(req).ok, true);
});

test('REALM is the documented dashboard realm', () => {
  assert.equal(REALM, 'agentteams-dashboard');
});