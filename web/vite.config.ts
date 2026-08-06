import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import ui from '@nuxt/ui/vite'

import { uiColors } from './src/theme.ts'

// https://vite.dev/config/
export default defineConfig({
  plugins: [
    vue(),
    ui({
      ui: {
        colors: uiColors,
      },
      // Without this, `i-lucide-*` icon props are resolved at RUNTIME by
      // @iconify/vue calling out to https://api.iconify.design — this is
      // Nuxt UI's Vite (non-Nuxt) integration, so there is no build step
      // that pre-bundles the icons this app actually uses unless told to
      // scan for them. That made every icon-only button (e.g. Settings'
      // gear) depend on an uncached third-party network request with no
      // offline fallback and no visible error on failure — exactly the
      // "icon silently doesn't render" bug. `scan: true` walks the source
      // for `i-<collection>-<name>` references at build time and inlines
      // their SVG data into the client bundle, so icons render with zero
      // runtime network dependency.
      icon: {
        clientBundle: {
          scan: true,
        },
      },
    }),
  ],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:3080',
        changeOrigin: true,
      },
    },
  },
})
