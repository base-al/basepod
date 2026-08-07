<script setup lang="ts">
// The compact BasePod mark — the wordmark's own "b", not a separate
// drawing. Lifted directly from web/public/wordmark.svg's path data:
// that file is one <path> built from per-letter subpaths (an outer
// contour plus a counter/hole for letters with one), and these are
// exactly the two subpaths that draw "b" — same coordinates, untouched.
// See BasepodWordmark.vue for the full wordmark and
// design-identity-report.md for how the subpaths were identified.
//
// The viewBox crop is optically, not mathematically, centered: a plain
// bounding-box crop (padding every side by the same amount) leaves the
// glyph looking bottom-heavy and pushed left, because the bowl doesn't
// start until roughly a third of the way down (empty space above-right)
// and its rounded edges read visually "lighter" than the stem's hard
// straight edges. This crop trims the top and left (where there's
// either dead space or a hard flat edge, so a tight crop still reads as
// centered) and adds a little extra room on the right and bottom (where
// the rounded bowl needs breathing room to not look cramped against the
// frame) — see design-identity-report.md for the exact numbers.
//
// Used wherever the full wordmark doesn't fit: the browser tab
// (favicon.svg — a static export of this same path), the collapsed/
// narrow header, and any other small surface.
withDefaults(
  defineProps<{
    size?: number
    tone?: 'accent' | 'neutral' | 'running' | 'deploying' | 'error'
  }>(),
  { size: 20, tone: 'accent' },
)

const TONE_CLASS = {
  accent: 'text-accent',
  neutral: 'text-content-primary',
  running: 'text-status-running',
  deploying: 'text-status-deploying',
  error: 'text-status-error',
} as const
</script>

<template>
  <svg role="img" aria-label="BasePod" :height="size" :width="size" viewBox="-5 -8 83 113" fill="none" :class="TONE_CLASS[tone]">
    <!-- One path, two subpaths (outer contour + counter) — the default
         nonzero fill-rule needs both together to render the hole; this
         is exactly how the source path combines them, just without the
         other six letters. -->
    <path
      d="M4.99219 0C6.44277 4.03146e-05 7.59495 0.469579 8.44824 1.4082C9.38675 2.26152 9.85645 3.45632 9.85645 4.99219V42.624C12.6724 38.6135 16.3843 35.4137 20.9922 33.0244C25.6855 30.5497 30.8911 29.3115 36.6084 29.3115C43.1789 29.3116 49.0672 30.8906 54.2725 34.0479C59.4775 37.1198 63.573 41.344 66.5596 46.7197C69.6316 52.0957 71.168 58.1552 71.168 64.8965C71.1679 71.723 69.5889 77.8243 66.4316 83.2002C63.3597 88.5761 59.1357 92.8427 53.7598 96C48.3838 99.0719 42.2826 100.608 35.4561 100.608C28.8001 100.608 22.7842 99.0719 17.4082 96C12.1176 92.928 7.89365 88.7466 4.73633 83.4561C1.66436 78.0801 0.0853623 72.0641 0 65.4082V4.99219C0 3.45619 0.42694 2.26154 1.28027 1.4082C2.21894 0.469561 3.45621 0 4.99219 0ZM35.4561 38.2725C30.5923 38.2725 26.1977 39.4238 22.2725 41.7275C18.3471 44.0315 15.2743 47.2321 13.0557 51.3281C10.8371 55.3387 9.72754 59.8619 9.72754 64.8965C9.72761 70.0162 10.8372 74.5813 13.0557 78.5918C15.2743 82.6025 18.3471 85.803 22.2725 88.1924C26.1977 90.4962 30.5923 91.6484 35.4561 91.6484C40.4052 91.6484 44.8424 90.4962 48.7676 88.1924C52.6929 85.803 55.7657 82.6025 57.9844 78.5918C60.2882 74.5813 61.4404 70.0162 61.4404 64.8965C61.4404 59.8619 60.2883 55.3387 57.9844 51.3281C55.7657 47.2321 52.6929 44.0315 48.7676 41.7275C44.8424 39.4237 40.4051 38.2725 35.4561 38.2725Z"
      fill="currentColor"
    />
  </svg>
</template>
