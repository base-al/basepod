<script setup lang="ts">
// Settings -> Account: password + active sessions (issue #3). Unchanged
// content from the pre-reorg single-page Settings.vue — only its wrapper
// changed, from AppShell directly to SettingsShell (AppShell plus the new
// Account/Instance/Users sub-nav).
import SettingsShell from '../../components/SettingsShell.vue'
import ChangePasswordForm from '../../components/ChangePasswordForm.vue'
import SessionsPanel from '../../components/SessionsPanel.vue'
</script>

<template>
  <SettingsShell>
    <div class="mb-6">
      <h2 class="font-mono text-lg font-semibold tracking-tight text-content-primary">Account</h2>
      <p class="mt-1 text-sm text-content-secondary">Password and active sessions.</p>
    </div>

    <div class="flex flex-col gap-6">
      <ChangePasswordForm />
      <SessionsPanel />
    </div>
  </SettingsShell>
</template>
