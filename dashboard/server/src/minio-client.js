'use strict';

// Tiny MinIO (S3-compatible) client: GetObject and ListObjectsV2 only, which
// is all the dashboard's task table / project browser / file browser need.
// No AWS SDK dependency -- signs requests itself via sigv4.js and speaks
// plain HTTP(S) via node:http / node:https.

const http = require('node:http');
const https = require('node:https');
const { URL } = require('node:url');
const { signRequest, canonicalQueryString } = require('./sigv4');

class MinioClient {
  /**
   * @param {Object} opts
   * @param {string} opts.endpoint   e.g. "http://127.0.0.1:9000"
   * @param {string} opts.accessKey
   * @param {string} opts.secretKey
   * @param {string} opts.bucket
   * @param {string} [opts.region]
   */
  constructor(opts) {
    this.endpointUrl = new URL(opts.endpoint);
    this.accessKey = opts.accessKey;
    this.secretKey = opts.secretKey;
    this.bucket = opts.bucket;
    this.region = opts.region || 'us-east-1';
  }

  _transport() {
    return this.endpointUrl.protocol === 'https:' ? https : http;
  }

  _request(method, path, query, { asBuffer = false, extraHeaders = {} } = {}) {
    const host = this.endpointUrl.host;
    const { headers } = signRequest({
      method,
      host,
      path,
      query,
      accessKey: this.accessKey,
      secretKey: this.secretKey,
      region: this.region,
    });

    // extraHeaders (e.g. If-None-Match / If-Modified-Since) are merged onto
    // the transport request AFTER signing and are deliberately NOT part of
    // signRequest's canonicalHeadersMap -- they must stay unsigned so the
    // SigV4 SignedHeaders list (host;x-amz-content-sha256;x-amz-date) never
    // changes shape (plan Milestone 3, Step 2). MinIO/S3 evaluate conditional
    // request headers independently of which headers were signed.
    const requestHeaders = { ...headers, ...extraHeaders };

    const qs = canonicalQueryString(query);
    const fullPath = qs ? `${path}?${qs}` : path;

    const transport = this._transport();

    return new Promise((resolve, reject) => {
      const req = transport.request(
        {
          protocol: this.endpointUrl.protocol,
          hostname: this.endpointUrl.hostname,
          port: this.endpointUrl.port || (this.endpointUrl.protocol === 'https:' ? 443 : 80),
          path: fullPath,
          method,
          headers: requestHeaders,
        },
        (res) => {
          const chunks = [];
          res.on('data', (c) => chunks.push(c));
          res.on('end', () => {
            const body = Buffer.concat(chunks);
            resolve({ statusCode: res.statusCode, headers: res.headers, body: asBuffer ? body : body.toString('utf8') });
          });
        },
      );
      req.on('error', reject);
      req.end();
    });
  }

  /**
   * getObject fetches an object by key. Returns { statusCode, body (Buffer), headers }.
   * Callers should treat statusCode 404 as "not found", 304 as "not modified"
   * (when conditionalHeaders were supplied), and anything >=400 (other than
   * 304) as an error.
   *
   * @param {string} key
   * @param {Object} [opts]
   * @param {{ 'if-none-match'?: string, 'if-modified-since'?: string }} [opts.conditionalHeaders]
   *   Forwarded verbatim as UNSIGNED request headers (plan Milestone 3, Step 2)
   *   -- never merged into the SigV4 signature.
   */
  async getObject(key, { conditionalHeaders } = {}) {
    const path = `/${this.bucket}/${key}`;
    return this._request('GET', path, {}, { asBuffer: true, extraHeaders: conditionalHeaders || {} });
  }

  /**
   * listObjects lists keys under a prefix (non-recursive, delimiter="/"),
   * returning { prefixes: string[], objects: [{key, size, lastModified}] }.
   */
  async listObjects(prefix) {
    const query = {
      'list-type': '2',
      prefix,
      delimiter: '/',
    };
    const path = `/${this.bucket}`;
    const res = await this._request('GET', path, query);
    if (res.statusCode >= 400) {
      const err = new Error(`minio list failed: ${res.statusCode}`);
      err.statusCode = res.statusCode;
      err.body = res.body;
      throw err;
    }
    return parseListObjectsV2(res.body);
  }
}

/** parseListObjectsV2 does a tiny, dependency-free XML scrape of the fields we need. */
function parseListObjectsV2(xml) {
  const prefixes = [...xml.matchAll(/<CommonPrefixes><Prefix>([^<]*)<\/Prefix><\/CommonPrefixes>/g)].map(
    (m) => decodeXml(m[1]),
  );
  const objects = [...xml.matchAll(/<Contents>([\s\S]*?)<\/Contents>/g)].map((m) => {
    const block = m[1];
    const key = (block.match(/<Key>([^<]*)<\/Key>/) || [])[1];
    const size = (block.match(/<Size>([^<]*)<\/Size>/) || [])[1];
    const lastModified = (block.match(/<LastModified>([^<]*)<\/LastModified>/) || [])[1];
    return {
      key: key ? decodeXml(key) : '',
      size: size ? Number(size) : 0,
      lastModified: lastModified || '',
    };
  });
  return { prefixes, objects };
}

function decodeXml(s) {
  return s
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, '&');
}

module.exports = { MinioClient, parseListObjectsV2 };
