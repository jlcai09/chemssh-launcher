import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { copyFileSync, existsSync, mkdirSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath, URL } from 'node:url'

const frontendRoot = fileURLToPath(new URL('.', import.meta.url))

function spaRoutesPlugin(): Plugin {
  return {
    name: 'chemssh-spa-routes',
    closeBundle() {
      const distDir = resolve(frontendRoot, 'dist')
      const indexHtml = resolve(distDir, 'index.html')
      if (!existsSync(indexHtml)) return
      for (const route of ['shell', 'terminal', 'sftp', 'launcher-logs']) {
        const routeDir = resolve(distDir, route)
        mkdirSync(routeDir, { recursive: true })
        copyFileSync(indexHtml, resolve(routeDir, 'index.html'))
      }
    }
  }
}

export default defineConfig({
  root: frontendRoot,
  base: '/',
  plugins: [vue(), spaRoutesPlugin()],
  server: {
    host: '127.0.0.1',
    port: 5174,
    strictPort: true,
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
    outDir: 'dist',
    emptyOutDir: true
  }
})
