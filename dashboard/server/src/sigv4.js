'use strict';

// Minimal AWS SigV4 request signer, just enough to call a MinIO S3-compatible
// endpoint's GetObject / ListObjectsV2 with a static access/secret key pair.
// Deliberately dependency-free (node:crypto only) per the brief's "prefer
// zero/low-dependency proxy" instruction.

const crypto = require('node:crypto');

function hmac(key, data) {
  return crypto.createHmac('sha256', key).update(data, 'utf8').digest();
}

function sha256hex(data) {
  return crypto.createHash('sha256').update(data).digest('hex');
}

function toAmzDate(date) {
  // YYYYMMDDTHHMMSSZ
  return date.toISOString().replace(/[:-]|\.\d{3}/g, '');
}

function toDateStamp(amzDate) {
  return amzDate.slice(0, 8);
}

function uriEncode(str, encodeSlash) {
  return encodeURIComponent(str)
    .replace(/[!'()*]/g, (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase())
    .replace(/%2F/g, encodeSlash ? '%2F' : '/');
}

function canonicalQueryString(query) {
  const keys = Object.keys(query).sort();
  return keys
    .map((k) => `${uriEncode(k, true)}=${uriEncode(String(query[k]), true)}`)
    .join('&');
}

/**
 * signRequest builds the headers needed to make a signed S3-compatible
 * request against MinIO.
 *
 * @param {Object} opts
 * @param {string} opts.method
 * @param {string} opts.host        host[:port], no scheme
 * @param {string} opts.path        path starting with "/", already URI-safe segments joined by "/"
 * @param {Object<string,string>} [opts.query]
 * @param {string} opts.accessKey
 * @param {string} opts.secretKey
 * @param {string} [opts.region]    defaults to "us-east-1" (MinIO ignores region but SigV4 needs one)
 * @param {string} [opts.service]   defaults to "s3"
 * @param {Date}   [opts.date]      defaults to now (injectable for tests)
 * @returns {{ headers: Object<string,string> }}
 */
function signRequest(opts) {
  const {
    method,
    host,
    path,
    query = {},
    accessKey,
    secretKey,
    region = 'us-east-1',
    service = 's3',
    date = new Date(),
  } = opts;

  const amzDate = toAmzDate(date);
  const dateStamp = toDateStamp(amzDate);
  const payloadHash = sha256hex('');

  const canonicalHeadersMap = {
    host,
    'x-amz-content-sha256': payloadHash,
    'x-amz-date': amzDate,
  };
  const signedHeaderNames = Object.keys(canonicalHeadersMap).sort();
  const canonicalHeaders = signedHeaderNames.map((k) => `${k}:${canonicalHeadersMap[k]}\n`).join('');
  const signedHeaders = signedHeaderNames.join(';');

  const canonicalUri = path
    .split('/')
    .map((seg) => (seg === '' ? '' : uriEncode(seg, true)))
    .join('/');

  const canonicalRequest = [
    method,
    canonicalUri,
    canonicalQueryString(query),
    canonicalHeaders,
    signedHeaders,
    payloadHash,
  ].join('\n');

  const credentialScope = `${dateStamp}/${region}/${service}/aws4_request`;
  const stringToSign = [
    'AWS4-HMAC-SHA256',
    amzDate,
    credentialScope,
    sha256hex(canonicalRequest),
  ].join('\n');

  const kDate = hmac(`AWS4${secretKey}`, dateStamp);
  const kRegion = hmac(kDate, region);
  const kService = hmac(kRegion, service);
  const kSigning = hmac(kService, 'aws4_request');
  const signature = crypto.createHmac('sha256', kSigning).update(stringToSign, 'utf8').digest('hex');

  const authorization =
    `AWS4-HMAC-SHA256 Credential=${accessKey}/${credentialScope}, ` +
    `SignedHeaders=${signedHeaders}, Signature=${signature}`;

  return {
    headers: {
      host,
      'x-amz-content-sha256': payloadHash,
      'x-amz-date': amzDate,
      authorization,
    },
  };
}

module.exports = { signRequest, canonicalQueryString, uriEncode };
