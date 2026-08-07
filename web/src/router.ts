import { createRouter, createWebHistory } from 'vue-router'

import { useAuthStore } from './stores/auth'

declare module 'vue-router' {
  interface RouteMeta {
    public?: boolean
  }
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('./pages/Login.vue'),
      meta: { public: true },
    },
    {
      path: '/',
      name: 'apps',
      component: () => import('./pages/Apps.vue'),
    },
    {
      path: '/apps/new',
      name: 'new-app',
      component: () => import('./pages/NewApp.vue'),
    },
    {
      path: '/apps/:slug',
      name: 'app-detail',
      component: () => import('./pages/AppDetail.vue'),
    },
    // Settings is a nested area (issue: IA reorg) — Account (password,
    // sessions), Instance (root domain, health, version, backup), and
    // Users (placeholder — a concurrent branch owns the backend). Each
    // sub-page is a real route so it's linkable and survives the back
    // button, per SettingsShell.vue's own doc comment. The bare /settings
    // path used to BE the page (just account content); it now redirects
    // to /settings/account so anything that bookmarked or linked the old
    // URL keeps working instead of landing on a blank/missing route.
    {
      path: '/settings',
      redirect: { name: 'settings-account' },
    },
    {
      path: '/settings/account',
      name: 'settings-account',
      component: () => import('./pages/settings/Account.vue'),
    },
    {
      path: '/settings/instance',
      name: 'settings-instance',
      component: () => import('./pages/settings/Instance.vue'),
    },
    {
      path: '/settings/users',
      name: 'settings-users',
      component: () => import('./pages/settings/Users.vue'),
    },
  ],
})

// No token -> bounce to /login. A token present is enough to pass the
// guard immediately; validity is checked lazily via auth.me(), which
// (through api.ts's single 401-interception point) will itself redirect
// to /login if the session turns out to be stale. This keeps navigation
// snappy instead of blocking every route change on a network round trip.
router.beforeEach((to) => {
  const auth = useAuthStore()

  if (to.meta.public) {
    if (to.name === 'login' && auth.token) {
      return { name: 'apps' }
    }
    return true
  }

  if (!auth.token) {
    return { name: 'login' }
  }

  void auth.me()
  return true
})
