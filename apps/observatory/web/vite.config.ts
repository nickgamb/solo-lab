import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// `npm run dev` proxies the API to a locally running server (go run . in ../server).
export default defineConfig({
  plugins: [react()],
  build: { outDir: '../server/web', emptyOutDir: true, chunkSizeWarningLimit: 4000 },
  server: { proxy: { '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false } } },
})
