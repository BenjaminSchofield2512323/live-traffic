import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Proxies to the Go API (backend/cmd/api). The detector on :8090 does not serve /api.
// If Vite logs "http proxy error" + ECONNREFUSED, start the Go server: `cd backend && go run ./cmd/api`
// (or `make dev` from repo root). Optional: VITE_API_PROXY_TARGET=http://127.0.0.1:PORT
const apiTarget = process.env.VITE_API_PROXY_TARGET || 'http://127.0.0.1:8080'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: apiTarget,
        changeOrigin: true,
      },
      '/healthz': {
        target: apiTarget,
        changeOrigin: true,
      },
      '/artifacts': {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
})
