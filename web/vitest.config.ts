import { defineConfig } from 'vitest/config'

// Separate from vite.config.ts on purpose: the unit tests here cover pure
// TS modules only (relativeTime, envparse, slugify) with no Vue rendering
// or Nuxt UI involved, so there's nothing in vite.config.ts's plugin list
// (@vitejs/plugin-vue, @nuxt/ui/vite) this run actually needs — pulling
// them in would just slow every `npm test` down for no benefit.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
