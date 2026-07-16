'use strict';

const http = require('node:http');
const path = require('node:path');
const { loadConfig, readTokenFile, invalidateTokenFile } = require('./config');
const { ControllerClient } = require('./controller-client');
const { MinioClient } = require('./minio-client');
const { createRequestHandler } = require('./handler');
const { createRequestLogger } = require('./request-log');
const { createStaticFileServer } = require('./static');

function main() {
  const config = loadConfig();

  const controllerClient = new ControllerClient({
    baseUrl: config.controllerUrl,
    getToken: () => readTokenFile(config.tokenFile),
    onUnauthorized: () => invalidateTokenFile(config.tokenFile),
  });

  const minioClient = new MinioClient({
    endpoint: config.minio.endpoint,
    accessKey: config.minio.accessKey,
    secretKey: config.minio.secretKey,
    bucket: config.minio.bucket,
  });

  const logWrite = createRequestLogger();

  const webDist = path.join(__dirname, '..', '..', 'web', 'dist');
  const staticFile = createStaticFileServer(webDist);

  const handler = createRequestHandler({ controllerClient, minioClient, logWrite, staticFile });

  const server = http.createServer((req, res) => {
    handler(req, res).catch((err) => {
      // Defense in depth: createRequestHandler already catches upstream
      // errors, this only fires on a truly unexpected bug.
      // eslint-disable-next-line no-console
      console.error('unhandled request error', err);
      if (!res.headersSent) {
        res.writeHead(500, { 'content-type': 'application/json' });
      }
      res.end(JSON.stringify({ error: 'internal error' }));
    });
  });

  server.listen(config.port, config.bindHost, () => {
    // eslint-disable-next-line no-console
    console.log(`hiclaw-dashboard proxy listening on ${config.bindHost}:${config.port} -> controller ${config.controllerUrl}`);
  });

  return server;
}

if (require.main === module) {
  main();
}

module.exports = { main };
