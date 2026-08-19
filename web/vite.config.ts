import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// During `npm run dev` the frontend runs on 5173 and proxies API and
// WebSocket traffic to the Go server, so cookies stay same-origin.
export default defineConfig({
  plugins: [react()],
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8114',
        changeOrigin: false,
        ws: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    // Keep the embedded bundle small enough to stay comfortable in a binary.
    chunkSizeWarningLimit: 900,
  },
})
