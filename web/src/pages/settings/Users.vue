<script setup lang="ts">
// Settings -> Users: the real user-management screen (replaces the
// placeholder that shipped ahead of the users/roles backend — see git
// history for that page's own doc comment on why it was deliberately
// empty). Backed by GET/POST/PATCH/DELETE /users* (internal/api's
// handlers, api/openapi.yaml's `users` tag).
//
// Every row action is gated by the CURRENT user's own role, read fresh
// from GET /auth/me (meQuery below) rather than trusted from the auth
// store, since a role change takes effect on that user's next request —
// there's no push channel telling an already-open tab its own role just
// changed. lib/roles.ts's predicates mirror internal/authz's rank table
// so the right controls render up front, but the server call is always
// what actually enforces it (a 403 `forbidden` is handled the same as
// any other mutation error below, never treated like a stale-session
// 401 — see lib/api.ts's requestRaw, which only clears the session on
// 401).
import { computed, reactive, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { useToast } from '@nuxt/ui/composables'

import { api, ApiError, type UserSummary } from '../../lib/api'
import { buildInviteAcceptUrl } from '../../lib/inviteUrl'
import { relativeTime } from '../../lib/relativeTime'
import { ROLE_LABELS, assignableRoles, isAdminOrAbove, isLastActiveOwner, isOwner, type Role } from '../../lib/roles'
import SettingsShell from '../../components/SettingsShell.vue'
import ConfirmDanger from '../../components/ConfirmDanger.vue'

const toast = useToast()
const queryClient = useQueryClient()

// --- Current user's own role (gates every control below) -----------------

const meQuery = useQuery({ queryKey: ['auth-me'], queryFn: () => api.me() })
const currentUser = computed(() => meQuery.data.value ?? null)
const currentRole = computed<Role | null>(() => currentUser.value?.role ?? null)

// admin+: can see the list, invite, disable/enable. owner: can additionally
// change roles and remove. A viewer/member never reaches the point of
// calling GET /users at all (see `enabled` below) — the access-denied
// state renders straight off the role this instance already told us,
// with no doomed request in between.
const canView = computed(() => currentRole.value !== null && isAdminOrAbove(currentRole.value))
const canManageRoles = computed(() => currentRole.value !== null && isOwner(currentRole.value))

// --- User list -------------------------------------------------------------

const usersQuery = useQuery({
  queryKey: ['users'],
  queryFn: () => api.listUsers(),
  enabled: canView,
})

const users = computed(() => usersQuery.data.value ?? [])

// A 403 here (role revoked out from under an already-open tab, between
// meQuery resolving and this query firing) reads exactly like "you don't
// have access" — not a broken screen, not an empty table implying no
// users exist.
const forbidden = computed(
  () => usersQuery.isError.value && usersQuery.error.value instanceof ApiError && usersQuery.error.value.status === 403,
)
const loadError = computed(() => usersQuery.isError.value && !forbidden.value)

function invalidateUsers() {
  return queryClient.invalidateQueries({ queryKey: ['users'] })
}

function isSelf(user: UserSummary): boolean {
  return currentUser.value !== null && user.email === currentUser.value.email
}

function lastOwner(user: UserSummary): boolean {
  return isLastActiveOwner(user, users.value)
}

const ROLE_BADGE_COLOR: Record<Role, 'neutral' | 'primary'> = {
  viewer: 'neutral',
  member: 'neutral',
  admin: 'neutral',
  owner: 'primary',
}

// --- Invite ------------------------------------------------------------

const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

const inviteOpen = ref(false)
const inviteForm = reactive<{ email: string; role: Role }>({ email: '', role: 'member' })
const inviteError = ref('')

interface RevealedInvite {
  email: string
  role: Role
  url: string
  expiresAt: string
}
const revealedInvite = ref<RevealedInvite | null>(null)

// Roles the CURRENT user may assign — an admin can invite up to admin,
// never owner (api/openapi.yaml's inviteUser description), mirrored by
// lib/roles.ts's assignableRoles. Reused below for the per-row role-
// change select too, since only an owner (top rank) ever reaches that
// control, for whom this is every role anyway.
const roleOptions = computed(() =>
  assignableRoles(currentRole.value ?? 'viewer').map((role) => ({ label: ROLE_LABELS[role], value: role })),
)

const emailValid = computed(() => EMAIL_PATTERN.test(inviteForm.email.trim()))
const inviteFormValid = computed(() => emailValid.value && roleOptions.value.some((o) => o.value === inviteForm.role))

function openInvite() {
  inviteForm.email = ''
  inviteForm.role = roleOptions.value.find((o) => o.value === 'member')?.value ?? roleOptions.value[0]?.value ?? 'viewer'
  inviteError.value = ''
  revealedInvite.value = null
  inviteOpen.value = true
}

function closeInvite() {
  inviteOpen.value = false
  // Nothing re-fetches this token (see InviteUserResponse's doc comment
  // in lib/api.ts) — dropping it from memory the moment the panel closes
  // is the whole point, same as GitPanel.vue's revealedSecret.
  revealedInvite.value = null
}

const inviteMutation = useMutation({
  mutationFn: () => api.inviteUser({ email: inviteForm.email.trim(), role: inviteForm.role }),
  onSuccess: (res) => {
    inviteError.value = ''
    revealedInvite.value = {
      email: res.email,
      role: res.role,
      url: buildInviteAcceptUrl(window.location.origin, res.token),
      expiresAt: res.expires_at,
    }
  },
  onError: (err) => {
    // 409 user_exists / 422 validation — shown verbatim, not reworded,
    // matching every other form in this app.
    inviteError.value = err instanceof ApiError ? err.message : 'Could not send invite — try again'
  },
})

function submitInvite() {
  if (!inviteFormValid.value || inviteMutation.isPending.value) return
  inviteError.value = ''
  inviteMutation.mutate()
}

async function copyText(value: string, label: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.add({ title: 'Copied', description: label, color: 'success', icon: 'i-lucide-check' })
  } catch {
    toast.add({ title: 'Could not copy', color: 'error', icon: 'i-lucide-alert-circle' })
  }
}

// --- Role change ---------------------------------------------------------

// Every row renders twice in the DOM at once — once in the desktop table,
// once in the mobile card list (see the two `lg:hidden`/`hidden lg:block`
// wrappers below) — CSS just hides whichever one the current viewport
// doesn't use; both stay mounted. A popover's floating content is
// teleported to the document body by the underlying library, so if its
// open/close state were keyed by email alone, opening the (visible)
// mobile popover would ALSO satisfy the (CSS-hidden) desktop row's own
// identical v-if, teleporting a second copy of the same popover with no
// real anchor (its trigger has zero size under `display:none`) — it
// floats to the page's top-left corner instead of disappearing. Keying
// every open/close ref by `${layout}:${email}` instead of just `email`
// keeps the two representations' state fully independent, so only the
// one actually clicked ever opens.
type Layout = 'desktop' | 'mobile'
function rowKey(layout: Layout, email: string): string {
  return `${layout}:${email}`
}

const roleEditKey = ref<string | null>(null)
const roleDraft = ref<Role>('viewer')
const roleChangeError = ref('')

function openRoleEdit(layout: Layout, user: UserSummary) {
  roleEditKey.value = rowKey(layout, user.email)
  roleDraft.value = user.role
  roleChangeError.value = ''
}

function closeRoleEdit() {
  roleEditKey.value = null
  roleChangeError.value = ''
}

const roleMutation = useMutation({
  mutationFn: (payload: { email: string; role: Role }) => api.changeUserRole(payload.email, payload.role),
  onSuccess: async () => {
    roleEditKey.value = null
    roleChangeError.value = ''
    await invalidateUsers()
    toast.add({ title: 'Role updated', color: 'success', icon: 'i-lucide-check' })
  },
  onError: (err) => {
    // 409 last_owner surfaces right here, next to the control that
    // triggered it — "an instance must keep at least one owner" is the
    // server's own message for that code.
    roleChangeError.value = err instanceof ApiError ? err.message : 'Could not change role — try again'
  },
})

function submitRoleChange(user: UserSummary) {
  if (roleDraft.value === user.role || roleMutation.isPending.value) return
  roleChangeError.value = ''
  roleMutation.mutate({ email: user.email, role: roleDraft.value })
}

// --- Disable / enable ------------------------------------------------------

// Keyed by `${layout}:${email}` — see roleEditKey's doc comment just
// above for why (this popover is duplicated across the desktop/mobile
// layouts exactly the same way).
const disableConfirmKey = ref<string | null>(null)
const disableError = ref('')

const disableMutation = useMutation({
  mutationFn: (email: string) => api.disableUser(email),
  onSuccess: async () => {
    disableConfirmKey.value = null
    disableError.value = ''
    await invalidateUsers()
    toast.add({ title: 'User disabled', description: 'Their sessions were revoked immediately.', color: 'success', icon: 'i-lucide-ban' })
  },
  onError: (err) => {
    // Shown inline in the same confirm popover rather than a toast, so a
    // 409 last_owner reads right next to the control it blocked.
    disableError.value = err instanceof ApiError ? err.message : 'Could not disable user — try again'
  },
})

const enableMutation = useMutation({
  mutationFn: (email: string) => api.enableUser(email),
  onSuccess: async () => {
    await invalidateUsers()
    toast.add({ title: 'User enabled', description: 'They can sign in again.', color: 'success', icon: 'i-lucide-check' })
  },
  onError: (err) => {
    toast.add({
      title: 'Could not enable user',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})

function openDisableConfirm(layout: Layout, user: UserSummary) {
  disableConfirmKey.value = rowKey(layout, user.email)
  disableError.value = ''
}

// --- Remove (owner-only, typed-email confirm — the harsher of the two
// destructive actions here: disabling is reversible via Enable, removing
// is not) ------------------------------------------------------------------

const removeTarget = ref<UserSummary | null>(null)

const removeMutation = useMutation({
  mutationFn: (email: string) => api.deleteUser(email),
  onSuccess: async () => {
    removeTarget.value = null
    await invalidateUsers()
    toast.add({ title: 'User removed', color: 'success', icon: 'i-lucide-trash-2' })
  },
  onError: (err) => {
    removeTarget.value = null
    toast.add({
      title: 'Could not remove user',
      description: err instanceof ApiError ? err.message : 'Something went wrong — try again',
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  },
})
</script>

<template>
  <SettingsShell>
    <div class="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h2 class="font-mono text-lg font-semibold tracking-tight text-content-primary">Users</h2>
        <p class="mt-1 text-sm text-content-secondary">Accounts and roles for this instance.</p>
      </div>
      <UButton
        v-if="canView"
        color="primary"
        variant="soft"
        icon="i-lucide-user-plus"
        class="tap44"
        @click="openInvite"
      >
        Invite user
      </UButton>
    </div>

    <!-- meQuery still resolving: don't flash the access-denied state for
         a role we haven't actually loaded yet. -->
    <div v-if="meQuery.isPending.value" class="flex items-center justify-center rounded-lg border border-line py-16 text-sm text-content-muted">
      Loading…
    </div>

    <UAlert
      v-else-if="meQuery.isError.value"
      color="error"
      variant="subtle"
      title="Couldn't verify your access"
      description="Check that the BasePod server is running and reachable, then reload."
    />

    <!-- A member/viewer who reaches this page (deep link, or a role
         change since the app was last loaded) sees exactly why, not an
         empty table or a broken screen. -->
    <div
      v-else-if="!canView"
      class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-24 text-center"
    >
      <UIcon name="i-lucide-lock" class="h-8 w-8 text-content-muted" aria-hidden="true" />
      <p class="text-sm font-medium text-content-secondary">You don't have access to this page</p>
      <p class="max-w-sm text-sm text-content-muted">
        Managing users requires the admin role or above.
        <template v-if="currentRole">Your current role is <span class="font-mono text-content-secondary">{{ ROLE_LABELS[currentRole] }}</span>.</template>
        Ask an owner or admin if you need access.
      </p>
    </div>

    <template v-else>
      <UAlert
        v-if="forbidden"
        color="error"
        variant="subtle"
        title="You don't have access to this page"
        description="Your role no longer permits managing users — sign out and back in, or ask an owner to check your role."
      />

      <UAlert
        v-else-if="loadError"
        color="error"
        variant="subtle"
        title="Couldn't load users"
        :description="usersQuery.error.value instanceof ApiError ? usersQuery.error.value.message : 'Check that the BasePod server is running and reachable.'"
      />

      <div v-else-if="usersQuery.isPending.value" class="flex flex-col gap-3" aria-busy="true" aria-label="Loading users">
        <div v-for="n in 3" :key="n" class="flex items-center gap-4 rounded-lg border border-line px-4 py-4 sm:px-5">
          <div class="h-3.5 w-40 animate-pulse rounded bg-line" />
          <div class="h-5 w-16 animate-pulse rounded-full bg-line" />
          <div class="hidden h-3.5 flex-1 animate-pulse rounded bg-line lg:block" />
        </div>
      </div>

      <div
        v-else-if="users.length === 0"
        class="flex flex-col items-center justify-center gap-3 rounded-lg border border-dashed border-line py-24 text-center"
      >
        <UIcon name="i-lucide-users" class="h-8 w-8 text-content-muted" aria-hidden="true" />
        <p class="text-sm font-medium text-content-secondary">No users yet</p>
        <p class="max-w-sm text-sm text-content-muted">Invite someone to give them their own sign-in and role on this instance.</p>
      </div>

      <template v-else>
        <!-- Wide desktop only (lg: 1024px, not Apps.vue's md: 768px):
             this table sits inside SettingsShell, which already spends
             width on AppShell's rail AND its own side-nav column before
             any table content starts — Apps.vue has neither, so its
             table can afford to switch on at md. Verified at 768px this
             5-column row (email, an editable role control, a state
             badge, joined date, three action icons) doesn't fit in
             what's left; cards read fine at any width, so they carry
             all the way through tablet instead of being squeezed. -->
        <div class="hidden overflow-hidden rounded-lg border border-line lg:block">
          <div
            class="grid grid-cols-[minmax(0,1.4fr)_auto_auto_auto_auto] items-center gap-4 border-b border-line bg-surface-elevated/40 px-5 py-2.5 text-xs font-medium uppercase tracking-wide text-content-muted"
          >
            <span>User</span>
            <span>Role</span>
            <span>State</span>
            <span>Joined</span>
            <span class="text-right">Actions</span>
          </div>

          <div
            v-for="user in users"
            :key="user.email"
            class="grid grid-cols-[minmax(0,1.4fr)_auto_auto_auto_auto] items-center gap-4 border-b border-line px-5 py-3.5 last:border-b-0"
          >
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <span class="truncate text-sm font-medium text-content-primary">{{ user.name || user.email }}</span>
                <UBadge v-if="isSelf(user)" color="neutral" variant="subtle" size="sm">You</UBadge>
              </div>
              <span class="block truncate font-mono text-xs text-content-muted">{{ user.email }}</span>
            </div>

            <div>
              <UPopover
                v-if="canManageRoles"
                :open="roleEditKey === rowKey('desktop', user.email)"
                @update:open="(v: boolean) => (v ? openRoleEdit('desktop', user) : closeRoleEdit())"
              >
                <UButton
                  size="xs"
                  :color="ROLE_BADGE_COLOR[user.role]"
                  variant="subtle"
                  class="tap44"
                  :disabled="lastOwner(user)"
                  :title="lastOwner(user) ? 'An instance must keep at least one owner' : 'Change role'"
                >
                  {{ ROLE_LABELS[user.role] }}
                  <UIcon name="i-lucide-chevron-down" class="ml-1 size-3" aria-hidden="true" />
                </UButton>
                <template #content>
                  <div class="flex w-56 flex-col gap-2 p-3">
                    <p class="text-xs text-content-secondary">Change <span class="font-mono">{{ user.email }}</span>'s role</p>
                    <USelect v-model="roleDraft" :items="roleOptions" class="tap44 w-full" :disabled="roleMutation.isPending.value" />
                    <p v-if="roleChangeError" class="text-xs text-status-error">{{ roleChangeError }}</p>
                    <div class="tap-row flex justify-end gap-2">
                      <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="closeRoleEdit">Cancel</UButton>
                      <UButton
                        size="xs"
                        color="primary"
                        class="tap44"
                        :loading="roleMutation.isPending.value"
                        :disabled="roleDraft === user.role"
                        @click="submitRoleChange(user)"
                      >
                        Save
                      </UButton>
                    </div>
                  </div>
                </template>
              </UPopover>
              <UBadge v-else :color="ROLE_BADGE_COLOR[user.role]" variant="subtle">{{ ROLE_LABELS[user.role] }}</UBadge>
            </div>

            <UBadge :color="user.disabled ? 'neutral' : 'success'" variant="subtle">{{ user.disabled ? 'Disabled' : 'Active' }}</UBadge>

            <span class="font-mono text-xs whitespace-nowrap text-content-secondary" :title="user.created_at">{{ relativeTime(user.created_at) }}</span>

            <div class="flex items-center justify-end gap-1.5">
              <UPopover
                v-if="canView && !user.disabled"
                :open="disableConfirmKey === rowKey('desktop', user.email)"
                @update:open="(v: boolean) => (v ? openDisableConfirm('desktop', user) : (disableConfirmKey = null))"
              >
                <UButton
                  size="xs"
                  color="warning"
                  variant="ghost"
                  square
                  class="tap44"
                  icon="i-lucide-ban"
                  :disabled="lastOwner(user)"
                  :aria-label="`Disable ${user.email}`"
                  :title="lastOwner(user) ? 'An instance must keep at least one owner' : 'Disable'"
                />
                <template #content>
                  <div class="flex w-64 flex-col gap-2 p-3">
                    <p class="text-xs text-content-secondary">
                      Disable <span class="font-mono">{{ user.email }}</span>? Their sessions end immediately and they can't sign in
                      again until re-enabled. The account itself is untouched.
                    </p>
                    <p v-if="disableError" class="text-xs text-status-error">{{ disableError }}</p>
                    <div class="tap-row flex justify-end gap-2">
                      <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="disableConfirmKey = null">Cancel</UButton>
                      <UButton
                        size="xs"
                        color="warning"
                        class="tap44"
                        :loading="disableMutation.isPending.value"
                        @click="disableMutation.mutate(user.email)"
                      >
                        Disable
                      </UButton>
                    </div>
                  </div>
                </template>
              </UPopover>

              <UButton
                v-else-if="canView && user.disabled"
                size="xs"
                color="success"
                variant="ghost"
                square
                class="tap44"
                icon="i-lucide-rotate-ccw"
                :loading="enableMutation.isPending.value"
                :aria-label="`Enable ${user.email}`"
                title="Enable"
                @click="enableMutation.mutate(user.email)"
              />

              <UButton
                v-if="canManageRoles"
                size="xs"
                color="error"
                variant="ghost"
                square
                class="tap44"
                icon="i-lucide-trash-2"
                :disabled="lastOwner(user)"
                :aria-label="`Remove ${user.email}`"
                :title="lastOwner(user) ? 'An instance must keep at least one owner' : 'Remove'"
                @click="removeTarget = user"
              />
            </div>
          </div>
        </div>

        <!-- Phone through tablet (< lg): cards, not a horizontally-
             scrolling table (issue #14 is open about exactly that
             regression elsewhere in this app — this list doesn't repeat
             it, and goes a step further than Apps.vue's own md-based
             split for the width reasons above). -->
        <div class="flex flex-col gap-3 lg:hidden">
          <div v-for="user in users" :key="user.email" class="flex flex-col gap-3 rounded-lg border border-line p-4">
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="truncate text-sm font-medium text-content-primary">{{ user.name || user.email }}</span>
                  <UBadge v-if="isSelf(user)" color="neutral" variant="subtle" size="sm">You</UBadge>
                </div>
                <span class="block truncate font-mono text-xs text-content-muted">{{ user.email }}</span>
              </div>
              <UBadge :color="user.disabled ? 'neutral' : 'success'" variant="subtle" class="shrink-0">{{ user.disabled ? 'Disabled' : 'Active' }}</UBadge>
            </div>

            <div class="flex items-center justify-between text-xs text-content-muted">
              <span class="font-mono">joined {{ relativeTime(user.created_at) }}</span>
              <UBadge :color="ROLE_BADGE_COLOR[user.role]" variant="subtle">{{ ROLE_LABELS[user.role] }}</UBadge>
            </div>

            <div class="tap-row flex flex-wrap items-center gap-2 border-t border-line pt-3">
              <UPopover
                v-if="canManageRoles"
                :open="roleEditKey === rowKey('mobile', user.email)"
                @update:open="(v: boolean) => (v ? openRoleEdit('mobile', user) : closeRoleEdit())"
              >
                <UButton size="xs" color="neutral" variant="soft" class="tap44" icon="i-lucide-shield" :disabled="lastOwner(user)">
                  Change role
                </UButton>
                <template #content>
                  <div class="flex w-56 flex-col gap-2 p-3">
                    <p class="text-xs text-content-secondary">Change <span class="font-mono">{{ user.email }}</span>'s role</p>
                    <USelect v-model="roleDraft" :items="roleOptions" class="tap44 w-full" :disabled="roleMutation.isPending.value" />
                    <p v-if="roleChangeError" class="text-xs text-status-error">{{ roleChangeError }}</p>
                    <div class="tap-row flex justify-end gap-2">
                      <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="closeRoleEdit">Cancel</UButton>
                      <UButton
                        size="xs"
                        color="primary"
                        class="tap44"
                        :loading="roleMutation.isPending.value"
                        :disabled="roleDraft === user.role"
                        @click="submitRoleChange(user)"
                      >
                        Save
                      </UButton>
                    </div>
                  </div>
                </template>
              </UPopover>

              <UPopover
                v-if="canView && !user.disabled"
                :open="disableConfirmKey === rowKey('mobile', user.email)"
                @update:open="(v: boolean) => (v ? openDisableConfirm('mobile', user) : (disableConfirmKey = null))"
              >
                <UButton size="xs" color="warning" variant="soft" class="tap44" icon="i-lucide-ban" :disabled="lastOwner(user)">
                  Disable
                </UButton>
                <template #content>
                  <div class="flex w-64 flex-col gap-2 p-3">
                    <p class="text-xs text-content-secondary">
                      Disable <span class="font-mono">{{ user.email }}</span>? Their sessions end immediately and they can't sign in
                      again until re-enabled.
                    </p>
                    <p v-if="disableError" class="text-xs text-status-error">{{ disableError }}</p>
                    <div class="tap-row flex justify-end gap-2">
                      <UButton size="xs" color="neutral" variant="ghost" class="tap44" @click="disableConfirmKey = null">Cancel</UButton>
                      <UButton
                        size="xs"
                        color="warning"
                        class="tap44"
                        :loading="disableMutation.isPending.value"
                        @click="disableMutation.mutate(user.email)"
                      >
                        Disable
                      </UButton>
                    </div>
                  </div>
                </template>
              </UPopover>

              <UButton
                v-else-if="canView && user.disabled"
                size="xs"
                color="success"
                variant="soft"
                class="tap44"
                icon="i-lucide-rotate-ccw"
                :loading="enableMutation.isPending.value"
                @click="enableMutation.mutate(user.email)"
              >
                Enable
              </UButton>

              <UButton
                v-if="canManageRoles"
                size="xs"
                color="error"
                variant="soft"
                class="tap44 ml-auto"
                icon="i-lucide-trash-2"
                :disabled="lastOwner(user)"
                @click="removeTarget = user"
              >
                Remove
              </UButton>
            </div>
          </div>
        </div>
      </template>
    </template>

    <!-- Invite modal: form, then (on success) the one-time token reveal. -->
    <UModal :open="inviteOpen" title="Invite a user" :dismissible="!inviteMutation.isPending.value" @update:open="(v: boolean) => (v ? (inviteOpen = true) : closeInvite())">
      <template #body>
        <div v-if="!revealedInvite" class="flex flex-col gap-4">
          <UFormField label="Email" name="invite-email">
            <UInput
              v-model="inviteForm.email"
              type="email"
              autocomplete="off"
              placeholder="teammate@example.com"
              class="w-full"
              :disabled="inviteMutation.isPending.value"
              autofocus
            />
          </UFormField>

          <UFormField label="Role" name="invite-role">
            <USelect v-model="inviteForm.role" :items="roleOptions" class="tap44 w-full" :disabled="inviteMutation.isPending.value" />
            <template #hint>
              <span class="text-xs text-content-muted">You can invite up to your own role — never higher.</span>
            </template>
          </UFormField>

          <UAlert v-if="inviteError" color="error" variant="subtle" :title="inviteError" icon="i-lucide-alert-circle" />
        </div>

        <div v-else class="flex flex-col gap-4">
          <div class="flex flex-col gap-2 rounded-md border border-moss-400/30 bg-status-running-bg p-3">
            <p class="flex items-center gap-1.5 text-xs font-medium text-status-running">
              <UIcon name="i-lucide-eye" class="size-3.5" aria-hidden="true" />
              Copy this link now — it won't be shown again
            </p>
            <div class="flex items-center gap-2">
              <span class="min-w-0 flex-1 truncate rounded-md border border-line bg-surface-sunken px-3 py-1.5 font-mono text-xs text-content-primary">
                {{ revealedInvite.url }}
              </span>
              <UButton size="sm" color="neutral" variant="ghost" square class="tap44" icon="i-lucide-copy" aria-label="Copy invite link" @click="copyText(revealedInvite.url, 'Invite link')" />
            </div>
          </div>

          <p class="text-xs text-content-secondary">
            Invited <span class="font-mono text-content-primary">{{ revealedInvite.email }}</span> as
            <span class="font-mono text-content-primary">{{ ROLE_LABELS[revealedInvite.role] }}</span>. Send them this link — it expires
            {{ relativeTime(revealedInvite.expiresAt) }}.
          </p>

          <p class="text-xs text-content-muted">
            There's no way to revoke an invite once sent — if it was sent by mistake, the only recourse is to let it expire.
          </p>
        </div>
      </template>

      <template #footer>
        <div class="tap-row flex w-full justify-end gap-2">
          <template v-if="!revealedInvite">
            <UButton color="neutral" variant="ghost" class="tap44" :disabled="inviteMutation.isPending.value" @click="closeInvite">Cancel</UButton>
            <UButton
              color="primary"
              class="tap44"
              icon="i-lucide-send"
              :loading="inviteMutation.isPending.value"
              :disabled="!inviteFormValid || inviteMutation.isPending.value"
              @click="submitInvite"
            >
              Send invite
            </UButton>
          </template>
          <UButton v-else color="neutral" variant="soft" class="tap44" @click="closeInvite">Done</UButton>
        </div>
      </template>
    </UModal>

    <ConfirmDanger
      :open="removeTarget !== null"
      :confirm-text="removeTarget?.email ?? ''"
      title="Remove this user?"
      :description="`Deletes ${removeTarget?.email}'s account permanently and revokes every one of their sessions. Disabling (reversible) is the lighter option if you just need to block them for now — this cannot be undone.`"
      confirm-label="Remove user"
      :loading="removeMutation.isPending.value"
      @update:open="(v: boolean) => (removeTarget = v ? removeTarget : null)"
      @confirm="removeTarget && removeMutation.mutate(removeTarget.email)"
    />
  </SettingsShell>
</template>
