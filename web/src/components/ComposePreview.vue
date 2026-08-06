<script setup lang="ts">
// NewApp.vue's Compose source: renders a ComposePlan — either a dry-run
// preview (plan.dry_run true, nothing has changed yet, a "Confirm &
// apply" footer is shown) or the result of a real apply (plan.dry_run
// false, per-service deployment numbers are real and pollable).
//
// Each card renders what the service will actually run — image (or build
// context for a `build:` service) and named volumes — plus its env var
// keys, so a reviewer can answer "is this the image I expect, is my
// database volume attached?" before anything is applied (issue #12).
// env_keys is exactly that: keys only. The API never sends a value (see
// internal/api/compose.go's composeServiceResponse doc comment) — this
// component has no value to render even if it wanted to.
import type { ComposePlan, ComposeService } from '../lib/api'
import { hasOrphans, planWarningCount, serviceActionLabel, servicePortLabel } from '../lib/composePreview'

const props = defineProps<{ plan: ComposePlan; confirming?: boolean }>()

const emit = defineEmits<{ confirm: [] }>()

function actionColor(action: string) {
  return action === 'update' ? 'warning' : 'success'
}

function statusFor(service: ComposeService): string {
  if (!props.plan.dry_run && service.deployment_number) {
    return `Deployment #${service.deployment_number}`
  }
  return ''
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <div>
        <h3 class="text-sm font-medium text-slate-200">
          Project <span class="font-mono text-slate-100">{{ plan.project }}</span>
        </h3>
        <p class="text-xs text-slate-500">
          {{ plan.services.length }} service{{ plan.services.length === 1 ? '' : 's' }} —
          {{ plan.dry_run ? 'preview only, nothing has been applied yet' : 'applied' }}
        </p>
      </div>
      <UBadge :color="plan.dry_run ? 'neutral' : 'success'" variant="subtle">
        {{ plan.dry_run ? 'Dry run' : 'Applied' }}
      </UBadge>
    </div>

    <!-- Warnings are rendered prominently, ahead of the per-service cards
         — the "warn loudly" ruling applies to this UI too (v0.5 doc 06). -->
    <UAlert
      v-if="planWarningCount(plan) > 0"
      color="warning"
      variant="subtle"
      icon="i-lucide-alert-triangle"
      :title="`${planWarningCount(plan)} warning${planWarningCount(plan) === 1 ? '' : 's'}`"
    >
      <template #description>
        <ul class="mt-1 flex flex-col gap-1">
          <li v-for="(w, i) in plan.warnings" :key="`top-${i}`" class="text-xs text-amber-300">{{ w }}</li>
        </ul>
      </template>
    </UAlert>

    <UAlert
      v-if="hasOrphans(plan)"
      color="warning"
      variant="subtle"
      icon="i-lucide-circle-slash"
      title="Orphaned services from a prior apply"
    >
      <template #description>
        <p class="mt-1 text-xs text-amber-300">
          These apps belong to this project but have no matching service in the file you're applying now. BasePod never
          deletes a running app as a side effect of a compose apply — they're still running exactly as before. Delete them
          explicitly (Apps list) if you no longer want them.
        </p>
        <ul class="mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5">
          <li v-for="slug in plan.orphans" :key="slug" class="font-mono text-xs text-amber-200">{{ slug }}</li>
        </ul>
      </template>
    </UAlert>

    <div class="grid gap-3 sm:grid-cols-2">
      <div v-for="service in plan.services" :key="service.name" class="flex flex-col gap-2 rounded-lg border border-slate-800 p-3">
        <div class="flex items-start justify-between gap-2">
          <div class="min-w-0">
            <p class="truncate font-mono text-sm font-medium text-slate-100">{{ service.name }}</p>
            <p class="truncate font-mono text-xs text-slate-500" :title="service.slug">{{ service.slug }}</p>
          </div>
          <UBadge :color="actionColor(service.action)" variant="subtle" size="sm">{{ serviceActionLabel(service.action) }}</UBadge>
        </div>

        <div class="flex flex-wrap items-center gap-1.5">
          <UBadge :color="service.internal ? 'neutral' : 'info'" variant="subtle" size="sm">
            {{ servicePortLabel(service) }}
          </UBadge>
          <UBadge v-if="service.deploy_strategy === 'replace'" color="warning" variant="subtle" size="sm">replace strategy</UBadge>
          <UBadge v-if="statusFor(service)" color="success" variant="subtle" size="sm" class="font-mono">
            {{ statusFor(service) }}
          </UBadge>
        </div>

        <!-- What this service will actually run: image (or build context
             for a `build:` service) and its named volumes — the fields a
             reviewer needs to check "is this the image I expect, is my
             database volume attached?" before applying (issue #12). -->
        <div class="flex flex-col gap-1 rounded-md border border-slate-800/60 bg-slate-900/40 px-2.5 py-2 text-xs">
          <div v-if="service.image" class="flex min-w-0 items-center gap-1.5">
            <UIcon name="i-lucide-box" class="size-3.5 shrink-0 text-slate-500" />
            <span class="truncate font-mono text-emerald-300" :title="service.image">{{ service.image }}</span>
          </div>
          <div v-else-if="service.build" class="flex min-w-0 items-center gap-1.5">
            <UIcon name="i-lucide-hammer" class="size-3.5 shrink-0 text-slate-500" />
            <span class="truncate font-mono text-emerald-300" :title="service.build.context">
              build: {{ service.build.context }}
            </span>
            <span v-if="service.build.dockerfile" class="shrink-0 truncate font-mono text-slate-500">
              ({{ service.build.dockerfile }})
            </span>
          </div>

          <div v-if="service.volumes?.length" class="flex flex-col gap-0.5">
            <div v-for="v in service.volumes" :key="v.name" class="flex min-w-0 items-center gap-1.5">
              <UIcon name="i-lucide-database" class="size-3.5 shrink-0 text-slate-500" />
              <span class="truncate font-mono text-slate-300">{{ v.name }}</span>
              <span class="shrink-0 text-slate-600">&rarr;</span>
              <span class="truncate font-mono text-slate-400" :title="v.path">{{ v.path }}</span>
            </div>
          </div>

          <div v-if="service.env_keys?.length" class="flex flex-wrap items-center gap-1 pt-0.5">
            <UIcon name="i-lucide-key-round" class="size-3.5 shrink-0 text-slate-500" />
            <UBadge v-for="key in service.env_keys" :key="key" color="neutral" variant="subtle" size="sm" class="font-mono">
              {{ key }}
            </UBadge>
          </div>
        </div>

        <ul v-if="service.warnings?.length" class="flex flex-col gap-1">
          <li v-for="(w, i) in service.warnings" :key="i" class="text-xs text-amber-400">{{ w }}</li>
        </ul>
      </div>
    </div>

    <div v-if="plan.dry_run" class="flex justify-end">
      <UButton color="primary" icon="i-lucide-rocket" :loading="confirming" :disabled="confirming" @click="emit('confirm')">
        Confirm &amp; apply
      </UButton>
    </div>
  </div>
</template>
