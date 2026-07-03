# HiClaw dashboard: same-origin proxy (dashboard/server) serving the built
# SPA (dashboard/web). Multi-stage build: compile the SPA with Node, then
# run only the proxy + static assets in the final image (no dev deps, no
# Vite at runtime).

FROM node:24-alpine AS web-build
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
RUN npm run build

FROM node:24-alpine
WORKDIR /app
ENV NODE_ENV=production
# The proxy is intentionally dependency-free (node:http + node:crypto only --
# see server/package.json), so there is no node_modules/ to copy here.
COPY server/package.json ./server/package.json
COPY server/src ./server/src
COPY --from=web-build /web/dist ./web/dist

# Defaults; override at deploy time. See README.md for the full list.
ENV PORT=8090 \
    HICLAW_CONTROLLER_URL=http://127.0.0.1:8080 \
    HICLAW_AUTH_TOKEN_FILE=/var/run/hiclaw/cli-token \
    MINIO_ENDPOINT=http://127.0.0.1:9000 \
    HICLAW_FS_BUCKET=hiclaw-storage

EXPOSE 8090

CMD ["node", "server/src/server.js"]
