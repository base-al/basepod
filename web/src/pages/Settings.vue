<script setup lang="ts">
// Settings -> Account: change password + manage active sessions (issue #3
// — until now the only way to change the admin password was editing
// SQLite directly, and a session could only be ended by its own logout).
import AppShell from '../components/AppShell.vue'
import ChangePasswordForm from '../components/ChangePasswordForm.vue'
import SessionsPanel from '../components/SessionsPanel.vue'
</script>

<template>
  <AppShell max-width="3xl">
    <div class="mb-6">
      <h1 class="font-mono text-2xl font-semibold tracking-tight text-content-primary">Settings</h1>
      <p class="mt-1 text-sm text-content-secondary">Account security — password and active sessions.</p>
    </div>

    <div class="flex flex-col gap-6">
      <ChangePasswordForm />
      <SessionsPanel />
    </div>
  </AppShell>
</template>
