<script setup lang="ts">
// Settings -> Users: placeholder. A concurrent branch owns the users/roles
// backend (auth is currently single-admin — see stores/auth.ts); this page
// only scaffolds the route and an honest empty state so the nav item and
// URL exist ahead of that work landing. Deliberately no fake rows, no
// disabled "Invite user" button implying a feature that isn't there — the
// task this page came from is explicit that faking data here would be
// worse than admitting it's not built yet.
import SettingsShell from '../../components/SettingsShell.vue'
</script>

<template>
  <SettingsShell>
    <div class="mb-6">
      <h2 class="font-mono text-lg font-semibold tracking-tight text-content-primary">Users</h2>
      <p class="mt-1 text-sm text-content-secondary">Accounts and roles for this instance.</p>
    </div>

    <div class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-24 text-center">
      <UIcon name="i-lucide-users" class="h-8 w-8 text-content-muted" aria-hidden="true" />
      <p class="text-sm font-medium text-content-secondary">Multi-user access is coming soon</p>
      <p class="max-w-sm text-sm text-content-muted">
        BasePod is currently single-admin — the credentials from <span class="font-mono">basepod setup</span> are the
        whole story (see Account for password and sessions). Inviting additional users with their own roles is being
        built next; this page will list them here once that lands.
      </p>
    </div>
  </SettingsShell>
</template>
