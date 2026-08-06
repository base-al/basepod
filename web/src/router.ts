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
    {
      path: '/settings',
      name: 'settings',
      component: () => import('./pages/Settings.vue'),
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
