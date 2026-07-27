import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Base "/admin/" matches where the Go control plane serves the built
// assets from (see cmd/serve.go's embed.FS wiring) -- otherwise the built
// bundle's asset URLs would resolve relative to "/" and 404.
export default defineConfig({
  base: '/admin/',
  plugins: [react(), tailwindcss()],
  build: {
    // Builds straight into the Go package that embeds it (see
    // internal/adminui/adminui.go) -- no separate copy step, and the
    // embed directive's relative path stays a plain "dist" subdirectory.
    outDir: '../internal/adminui/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // Forward /admin-api/* as-is -- the Go server expects that prefix
      // (do NOT strip it; a prior rewrite to "/" made every admin call 404).
      '/admin-api': {
        target: 'http://localhost:2026',
      },
    },
  },
})
