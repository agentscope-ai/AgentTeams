import { defineConfig } from 'vite';

// Dashboard SPA build config. No framework, no plugins -- Vite's default
// vanilla-JS pipeline is enough for the v1 read-only dashboard + v1.1
// wake/sleep actions. The build output (dist/) is served by the Node
// proxy (dashboard/server) as static files, same-origin with the /api/*
// routes, so no dev-server proxy config is needed for production.
export default defineConfig({
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // Local `vite dev` convenience only: forwards /api/* to a locally
      // running dashboard/server proxy so `npm run dev` works without a
      // separate CORS setup. Production never uses this -- it's same-origin.
      '/api': 'http://127.0.0.1:8090',
    },
  },
});
