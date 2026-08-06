import { defineStore } from 'pinia'
import { ref } from 'vue'

import { api, type User } from '../lib/api'
import { storageKeys } from '../theme'

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string | null>(localStorage.getItem(storageKeys.token))
  const user = ref<User | null>(null)
  const pending = ref(false)

  function setToken(next: string | null) {
    token.value = next
    if (next) {
      localStorage.setItem(storageKeys.token, next)
    } else {
      localStorage.removeItem(storageKeys.token)
    }
  }

  /** Clears local session state. Called by api.ts on a 401 (session
   * expired/invalid) and by logout() below. */
  function clearSession() {
    setToken(null)
    user.value = null
  }

  async function login(email: string, password: string) {
    pending.value = true
    try {
      const res = await api.login(email, password)
      setToken(res.token)
      user.value = res.user
    } finally {
      pending.value = false
    }
  }

  /** Revokes the session server-side (best-effort — a failed/expired call
   * is ignored, since the local session is cleared either way) before
   * clearing local state. */
  async function logout() {
    try {
      await api.logout()
    } catch {
      // Best-effort: the local session is cleared regardless of whether
      // the server-side revoke succeeded (e.g. token already expired, or
      // a network blip).
    }
    clearSession()
  }

  /** Bootstraps/validates the current session against GET /auth/me. Safe
   * to call speculatively (e.g. from the router guard) — a 401 is handled
   * globally by api.ts, so failures here are swallowed rather than thrown. */
  async function me() {
    if (!token.value) return
    try {
      user.value = await api.me()
    } catch {
      // api.ts already cleared the session and redirected on 401; any
      // other failure (network blip) just leaves `user` unset for now.
    }
  }

  return { token, user, pending, login, logout, me, clearSession }
})
