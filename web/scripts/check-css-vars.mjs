// Fails the build when the emitted CSS references a custom property that
// nothing defines.
//
// Why this needs its own gate: an unresolvable var() is silent. The
// browser drops the declaration and the property falls back to its
// inherited or initial value, so `border-color: var(--undefined)` becomes
// `currentColor` and `background-color: var(--undefined)` becomes
// transparent. Nothing errors, nothing logs — the page just renders
// wrong. This has shipped twice: dark-mode surfaces went transparent
// (#15), then every Nuxt UI border and divider rendered white, because
// Tailwind pruned the --ui-color-neutral-* and --ui-* role aliases while
// keeping the rules that depend on them. The scanner cannot see a var()
// chain as a "use", so pruning breaks the chain without warning.
//
// Only project-owned prefixes are checked. --tw-* is excluded: Tailwind
// registers those via @property with its own initial values, so they
// resolve without a textual definition.
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

const DIST = new URL('../dist/assets/', import.meta.url).pathname
const PREFIXES = /^--(ui|color|bp|radius|ease|font)-/

// var(--name) and var(--name, fallback). A declared fallback makes the
// reference safe on its own, so those are not required to be defined.
const REFERENCE = /var\(\s*(--[a-zA-Z0-9-]+)\s*([,)])/g
const DEFINITION = /(--[a-zA-Z0-9-]+)\s*:/g

const files = readdirSync(DIST).filter((f) => f.endsWith('.css'))
if (files.length === 0) {
  console.error('check-css-vars: no CSS found in dist/assets — did the build emit anything?')
  process.exit(1)
}

let failed = false
for (const file of files) {
  const css = readFileSync(join(DIST, file), 'utf8')

  const defined = new Set()
  for (const [, name] of css.matchAll(DEFINITION)) defined.add(name)

  const missing = new Set()
  for (const [, name, next] of css.matchAll(REFERENCE)) {
    if (next === ',') continue // has a fallback
    if (!PREFIXES.test(name)) continue
    if (!defined.has(name)) missing.add(name)
  }

  if (missing.size > 0) {
    failed = true
    console.error(
      `check-css-vars: ${file} references ${missing.size} custom ` +
        `${missing.size === 1 ? 'property' : 'properties'} that nothing defines:\n` +
        [...missing].sort().map((n) => `  ${n}`).join('\n') +
        '\nDeclarations using these are dropped by the browser and fall back to ' +
        'currentColor / transparent. If a Nuxt UI token is missing, define it in ' +
        'plain CSS in src/assets/css/main.css — outside @theme, where Tailwind ' +
        'cannot prune it.',
    )
  }
}

if (failed) process.exit(1)
console.log(`check-css-vars: ${files.length} file(s) OK — every custom property resolves`)
