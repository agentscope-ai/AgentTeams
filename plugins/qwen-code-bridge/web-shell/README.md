# Rebuilt Qwen Code web shell (base path)

The stock `qwen serve` web shell is an **origin-root** app (assets at `/assets/`,
daemon API at `/sessions`, `/workspace(s)`, …). Embedding it under a path prefix
breaks those absolute calls (including calls made from workers), and QwenPaw
plugins can only mount routes under `/api` — so the shell is rebuilt with a
**base path** instead of patched at runtime.

## Patch (3 files, source: `SC/qwen-code`, tag/version matching the installed qwen CLI)

1. `packages/web-shell/vite.config.ts`
   ```diff
   +  base: process.env['WEB_SHELL_BASE'] ?? '/',
   ```
2. `packages/web-shell/client/config/daemon.ts` — add an injectable base:
   ```ts
   export function daemonBaseOrigin(): string {
     if (typeof window === 'undefined') return '';
     const injected = (window as any).__QWEN_DAEMON_BASE__;
     if (typeof injected === 'string' && injected.trim()) return injected.trim().replace(/\/+$/, '');
     return window.location.origin;
   }
   ```
3. Use `daemonBaseOrigin()` instead of `window.location.origin` **wherever it
   means "the daemon base"**: `client/main.tsx` (2 sites), `client/App.tsx`
   (workspace origin sites), `client/utils/splitUrl.ts`.

The plugin injects `window.__QWEN_DAEMON_BASE__ = '/api/qwen-code-bridge/web'`
into the served `index.html`.

## Build

```bash
# local disk only (the bind mount must not carry node_modules/build trees)
git archive --format=tar HEAD | tar -x -C /root/qc-build
cd /root/qc-build
pnpm install --ignore-scripts
pnpm --filter "@qwen-code/sdk..." run build
cd packages/web-shell
WEB_SHELL_BASE=/api/qwen-code-bridge/web/ pnpm exec vite build
# install the artifact where the plugin serves it from
rm -rf <qwenpaw-working>/.qwenpaw/qwen-code-bridge/web-shell/dist
cp -a dist <qwenpaw-working>/.qwenpaw/qwen-code-bridge/web-shell/
```

The artifact is ~35 MB and is **not** committed; rebuild on upstream bumps.

## Proxy behavior (plugin `web.py`)

- `GET /web/*`: static file from the dist if present, else proxy to the daemon;
  upstream HTML (SPA) is replaced with our `index.html` so client routes work.
- Workspace selectors emitted as `…/workspaces//tmp/sessions` are re-encoded to
  `…/workspaces/%2Ftmp/sessions` (the daemon expects one segment).
- The daemon stays on loopback; the plugin injects the bearer token server-side,
  so no token reaches the browser.
