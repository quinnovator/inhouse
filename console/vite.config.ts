import { tanstackStart } from '@tanstack/react-start/plugin/vite'
import viteReact from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// The console is a single-page app: the build prerenders one shell page and
// inhoused embeds dist/client (see scripts/embed.mjs). Nothing runs on a
// server, so there are no server functions; data comes from /v1 in the
// browser, under the caller's own tailnet identity.
export default defineConfig({
  server: {
    port: 3000,
    // `go run ./internal/console/preview` serves the API with fake pods.
    proxy: { '/v1': 'http://127.0.0.1:8484' },
  },
  resolve: { tsconfigPaths: true },
  plugins: [tanstackStart({ spa: { enabled: true } }), viteReact()],
})
