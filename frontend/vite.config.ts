import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

const frontendRoot = fileURLToPath(new URL('.', import.meta.url))

export default defineConfig({
  root: frontendRoot,
  base: '/static/vue/',
  plugins: [vue()],
  server: {
    host: '127.0.0.1',
    port: 5174,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8888',
        changeOrigin: true
      },
      '/chemssh': {
        target: 'http://127.0.0.1:8888',
        changeOrigin: true
      }
    }
  },
  build: {
    outDir: '../internal/gui/static/vue',
    emptyOutDir: true
  }
})
