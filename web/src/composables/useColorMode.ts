import { ref } from 'vue'
import { storageKeys } from '../theme'

export type ColorMode = 'dark' | 'light'

// Module-level so every caller shares one reactive source of truth — this
// isn't a Pinia store because it's pure UI/DOM state, not app data.
const mode = ref<ColorMode>(
  document.documentElement.classList.contains('light') ? 'light' : 'dark',
)

function apply(next: ColorMode) {
  document.documentElement.classList.toggle('dark', next === 'dark')
  document.documentElement.classList.toggle('light', next === 'light')
  localStorage.setItem(storageKeys.colorMode, next)
  mode.value = next
}

/**
 * Dark/light toggle for BasePod's Nuxt UI theme. Dark is the default (see
 * theme.ts); this only ever needs to flip the `dark`/`light` class on
 * <html>, which the index.html inline script already seeds pre-paint.
 */
export function useColorMode() {
  function toggle() {
    apply(mode.value === 'dark' ? 'light' : 'dark')
  }

  return { mode, toggle }
}
