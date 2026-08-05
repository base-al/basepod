# BasePod web

Vue 3 + Vite dashboard for BasePod, built against the REST API under
`internal/api/` (see the repo root `README.md` for the API itself).

## Scripts

- `npm run dev` — Vite dev server; proxies `/api` to `http://localhost:3080`.
- `npm run build` — type-checks (`vue-tsc -b`) and builds to `dist/`.
- `npm run type-check` — `vue-tsc --noEmit`, no build output.
- `npm run preview` — serve the production build locally.

## Stack

- [@nuxt/ui](https://ui.nuxt.com) in its plain-Vue/Vite mode (not the Nuxt framework) for components and theming.
- [Pinia](https://pinia.vuejs.org) for the auth store.
- [vue-router](https://router.vuejs.org) for routing and the auth guard.
- [@tanstack/vue-query](https://tanstack.com/query/latest/docs/framework/vue/overview) for server-state fetching/polling.

The visual system (colors, status semantics, fonts) lives in `src/theme.ts`
and `src/assets/css/main.css`.
