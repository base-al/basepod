<script setup lang="ts">
// Settings -> Account: change password + manage active sessions (issue #3
// — until now the only way to change the admin password was editing
// SQLite directly, and a session could only be ended by its own logout).
import ChangePasswordForm from '../components/ChangePasswordForm.vue'
import SessionsPanel from '../components/SessionsPanel.vue'
</script>

<template>
  <div class="min-h-screen bg-slate-950">
    <header class="sticky top-0 z-10 border-b border-slate-800 bg-slate-950/90 backdrop-blur">
      <div class="mx-auto flex max-w-3xl items-center gap-3 px-6 py-3">
        <UButton to="/" color="neutral" variant="ghost" square size="sm" icon="i-lucide-arrow-left" aria-label="Back to apps" />
        <span class="text-base font-semibold tracking-tight text-slate-100">Settings</span>
      </div>
    </header>

    <main class="mx-auto flex max-w-3xl flex-col gap-6 px-6 py-8">
      <h1 class="text-sm font-medium text-slate-400">Account</h1>
      <ChangePasswordForm />
      <SessionsPanel />
    </main>
  </div>
</template>
