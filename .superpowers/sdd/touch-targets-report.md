# Touch-target and mobile-ergonomics pass

Scope: every file under `web/` except `web/src/pages/Apps.vue`, `web/src/lib/api.ts`, and Go — those belong to a
concurrent agent and were not touched.

## Approach

Two techniques, chosen per element:

1. **`.tap44`** (`web/src/assets/css/main.css`) — a reusable CSS utility that expands the *hit area* past an
   element's visual box via an invisible `::after` overlay sized to `max(100%, 44px)` on each axis, centered on the
   element. Gated to `@media (pointer: coarse)` — the actual "this is a finger" signal — so a mouse/trackpad user
   keeps the exact original compact box at any window size, and only touch input gets the larger tappable region.
   Used for essentially every icon-only button, small ghost/xs button, switch, and select in the app — the dense
   desktop sizing (Nuxt UI's `sm`/`xs` buttons render ~28-34px tall) never changes visually.
2. **Direct size bump under `pointer-coarse:`** (Tailwind's built-in coarse-pointer variant) — used instead of
   `.tap44` in exactly two places where the overlay technique would create ambiguity between *tap* and *scroll/swipe*
   gestures: the AppDetail tab strip (`pointer-coarse:py-3` on each trigger) and the NewApp source `URadioGroup`
   fieldset spacing. Both sit in a horizontally-scrolling or tightly-packed row where an invisible hit-area overlap
   between neighbors is worse than a slightly taller visible control.
3. **`.tap-row`** — a companion utility that widens `gap` to `0.5rem` under coarse pointer only, for rows of
   adjacent icon buttons (table actions, popover footers) where two `.tap44` hit areas would otherwise overlap
   heavily.

## Fixed-element table

| File | Element | Before → After | Technique |
|---|---|---|---|
| `AppShell.vue` | Desktop rail theme toggle / logout | ~28px box → 44px hit area | `.tap44` |
| `AppShell.vue` | Desktop rail nav items (Apps/Settings) | ~36px tall → 44px hit area | `.tap44` + `pointer-coarse:gap-1` on `<nav>` |
| `AppShell.vue` | Desktop wordmark home link | already generous | left as-is (compliant) |
| `AppShell.vue` | Mobile top-bar theme toggle / logout | ~28px box → 44px hit area | `.tap44` |
| `AppShell.vue` | Mobile top-bar wordmark home link | ~30px tall → 44px hit area | `.tap44` |
| `AppShell.vue` | Bottom tab bar items | already 56px, `flex-1` width | verified compliant, no change |
| `LogViewer.vue` | Pause/Resume, Clear, Download | ~28px → 44px hit area | `.tap44` + `.tap-row` (widened gap) |
| `LogViewer.vue` | Follow switch, tail-length select | ~20-32px → 44px hit area | `.tap44` |
| `LogViewer.vue` | Jump-to-latest floating button | ~30px → 44px hit area | `.tap44` |
| `LogViewer.vue` | Retry / "Go to Overview" buttons | ~30px → 44px hit area | `.tap44` |
| `BuildLogPanel.vue` | Retry button | ~24px → 44px hit area | `.tap44` |
| `EnvEditor.vue` | Per-row secret `USwitch` | ~20px → 44px hit area | `.tap44` |
| `EnvEditor.vue` | Per-row remove button (both the never-saved and popover-confirmed variants) | ~28px icon button → 44px hit area | `.tap44` |
| `EnvEditor.vue` | "Replace value" button | ~30px → 44px hit area | `.tap44` |
| `EnvEditor.vue` | Bulk edit / Add variable / Save / modal Cancel-Demote buttons | ~28-32px → 44px hit area | `.tap44` |
| `EnvEditor.vue` | Row layout (key/value/switch+remove) | wrap-based single row that could cram switch+remove together on narrow widths | restructured to `flex-col` below `sm`, `sm:contents` wrappers restore the original flat row at `sm:` — no desktop change |
| `DeploymentList.vue` | Error-detail native `<button>` | no focus ring, ~20px tall | `.tap44` + added `focus-visible:outline` (was completely missing — this one wasn't a Nuxt UI component so it never inherited the automatic focus ring) |
| `DeploymentList.vue` | Build log / Roll back buttons, popover Cancel/Roll back | ~24-28px → 44px hit area | `.tap44` + `.tap-row` |
| `DomainsPanel.vue` | Copy/open-external icon buttons (generated + custom domains) | ~28px → 44px hit area | `.tap44` + `.tap-row` |
| `DomainsPanel.vue` | Delete button + popover Cancel/Remove | ~28px → 44px hit area | `.tap44` + `.tap-row` |
| `DomainsPanel.vue` | Add domain submit | ~32px → 44px hit area | `.tap44` |
| `GitPanel.vue` | Edit, Copy (webhook URL + secret), Rotate, "Done, hide it", Refresh, Deploy now, Disconnect, Connect/Save/Cancel | ~24-32px → 44px hit area | `.tap44` + `.tap-row` on the footer rows |
| `GitPanel.vue` | "Deploy now" and "Disconnect" description+button rows | could overflow horizontally on very narrow widths (flex item min-width:auto) | added `flex-wrap`, `min-w-0 flex-1 break-words` on the paragraph |
| `AppDetail.vue` | "Apps" breadcrumb link | ~24px → 44px hit area | `.tap44` |
| `AppDetail.vue` | Generated-domain external link | no focus ring, ~18px tall | `.tap44` + added `focus-visible:outline` (was missing entirely) |
| `AppDetail.vue` | Deploy / Deploy new image / Delete / Deploy image / Cancel / Delete app | ~30-34px → 44px hit area | `.tap44` + `.tap-row` |
| `AppDetail.vue` | Danger-zone description+button row | same overflow risk as GitPanel's | `flex-wrap`, `min-w-0 flex-1 break-words` |
| `AppDetail.vue` | Tab strip triggers (Overview…Settings) | ~32-34px tall | `pointer-coarse:py-3` (direct size, not overlay — see rationale above) |
| `AppDetail.vue` | Tab strip scroll-into-view | programmatic tab changes (`?buildLog=`, Git "Deploy now") could land on an off-screen trigger | added a `watch(activeTab)` that calls `scrollIntoView({block:'nearest', inline:'nearest'})` on the active `[role="tab"]` |
| `ConfirmDanger.vue` | Cancel / destructive-confirm buttons | ~32px → 44px hit area | `.tap44` + `.tap-row` |
| `ChangePasswordForm.vue` | Change password submit | ~32px → 44px hit area | `.tap44` |
| `SessionsPanel.vue` | Revoke + popover Cancel/Revoke | ~24px → 44px hit area | `.tap44` + `.tap-row` |
| `ResourceLimitsPanel.vue` | 3× Unlimited `USwitch`, Save limits | ~20-32px → 44px hit area | `.tap44` |
| `ComposePreview.vue` | Confirm & apply | ~32px → 44px hit area | `.tap44` |
| `NewApp.vue` | Back-arrow icon button | ~28px → 44px hit area | `.tap44` |
| `NewApp.vue` | Cancel/Create&deploy, Cancel/Preview plan, Back/Done | ~32-34px → 44px hit area | `.tap44` + `.tap-row` |
| `NewApp.vue` | Source `URadioGroup` items | ~20px tall label row | `.tap44` on each item, `pointer-coarse:gap-y-3` on the fieldset |
| `Login.vue` | Sign in (block button) | ~40px → 44px hit area | `.tap44` |

75 `UButton` instances, every `USwitch`, and every `USelect` in the owned files now carry `.tap44` (verified by a
script that parsed every opening tag and flagged any missing the class — zero remaining after the pass).

## Phone experience at 390px, both themes

- **Top bar** (`<390px` and `414px`): wordmark + status dot on the left, theme toggle + logout on the right, all
  within the safe-area-aware sticky header. No overflow in either theme — colors are token-driven, no hardcoded
  light-only or dark-only values were touched.
- **Bottom tab bar**: already correctly using `.pb-safe` (`padding-bottom: max(0.5rem, env(safe-area-inset-bottom))`)
  and already last in DOM order after `<main>` (verified — the desktop `<aside>` is `display:none` on mobile via
  Tailwind's `hidden` class, so it's out of the phone's tab order entirely; `<header>` → `<main>` → bottom `<nav>` is
  the real order). No fix was needed here — flagged as "verify" in the brief, confirmed already correct.
- **AppDetail tab strip**: scrolls horizontally with `overflow-x-auto` + hidden scrollbar (pre-existing); now the
  active trigger is guaranteed on-screen after a programmatic tab change (fixed above), and each trigger has a real
  44px tap height under a coarse pointer without inflating desktop's tab height.
- **EnvEditor rows**: now stack vertically (key / value-or-masked+replace / switch+remove) below `sm`, instead of
  the old `flex-wrap` row that could dump the switch and remove button — the two smallest, hardest-to-hit controls —
  next to each other on a half-filled second line. `sm:contents` on the two inner wrappers means desktop's layout is
  byte-for-byte the same flat row it was before.
- **LogViewer**: mono lines already wrap (`whitespace-pre-wrap break-all`, pre-existing) so there's no horizontal
  overflow at any width. The control bar wraps via `flex-wrap` (pre-existing) so Follow/tail-select/Pause/Clear/
  Download never force a scrollbar. Pause is first in its cluster, closest to the natural one-handed-thumb rest
  position at the bottom-right on a right-handed grip. The log container's height is a fixed `32rem` inside normal
  page scroll (not `100vh`/`100dvh`), so it isn't affected by the on-screen keyboard or mobile browser chrome
  resizing the viewport — there's also no text input in this panel to summon a keyboard in the first place, so the
  "survives the keyboard" risk named in the brief doesn't actually apply here; verified rather than needing a fix.
- **BuildLogPanel**: same wrapping behavior, `max-h-96 overflow-y-auto`, no horizontal overflow.
- **GitPanel / AppDetail "Deploy now" and danger-zone rows**: previously `justify-between` with no wrap — a long
  branch name or narrow-enough viewport could force the paragraph and button to fight for space with no escape valve
  (flex items don't shrink below content by default). Now `flex-wrap` with `min-w-0 flex-1 break-words` on the text.
- **Tables** (DeploymentList, SessionsPanel, GitPanel deliveries): Nuxt UI's `UTable` root already carries
  `overflow-x-auto` (confirmed by reading `Table.vue`'s theme, not assumed) — horizontal scroll is contained to the
  table itself, never the page.
- Both themes were checked by reading the token system (`main.css`'s `:root` / `:root.light` blocks) — none of the
  changes above touch color, so there's nothing theme-specific to regress. The `.tap44`/`.tap-row`/`pointer-coarse:`
  rules are all layout-only.

## Keyboard and focus

- Nuxt UI's `UButton`/`ULink`/form components already ship an automatic `focus-visible:outline-3` in the
  component's own accent/error/warning color (confirmed by reading the compiled theme, not assumed) — no changes
  needed there.
- Two elements were bare HTML (`<button>`/`<a>`) that predate/bypass Nuxt UI and had **no** focus-visible styling at
  all: `DeploymentList.vue`'s error-detail toggle button, and `AppDetail.vue`'s generated-domain external link. Both
  now carry the same `focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2
  focus-visible:outline-accent` convention already used elsewhere in the app (AppShell's nav links, etc.).
- DOM/tab order on the phone layout was already correct: desktop `<aside>` is `display:none` (removed from tab
  order) below `md:`, and the bottom nav `<nav>` is the last element in the DOM, after `<main>` — verified, not
  changed.

## Verification

- `npm run type-check` — clean, no errors.
- `npm test` — 68/68 passing, no test file touched or needed changing (no assertion depended on button sizing or
  class lists).
- `npm run build` — clean production build (`vue-tsc -b && vite build`), no warnings besides Vite's normal plugin-
  timing notice.
- Served `web/dist` with `python3 -m http.server` and curled `index.html`, the JS entry, the CSS bundle, a
  `.woff2` font, and `favicon.svg` — all `200`, all same-origin, no 404s in the access log.
- Confirmed via the compiled CSS that `.tap44`, `.tap-row`, and every `pointer-coarse:*` utility used actually made
  it into the bundle (`@media (pointer:coarse){...}` block present with all three custom rules plus the three
  Tailwind-generated ones).
- **Important:** `web/dist/index.html` is intentionally checked into the repo as a placeholder
  (`embed.go`'s doc comment: "the built output must never be committed"). Running `npm run build` overwrote it with
  the real build for local verification; it was reverted with `git checkout -- web/dist/index.html` before
  committing. The `web/dist/assets/*` output itself is gitignored and was left in the working tree (harmless,
  untracked).
- No headless-browser console check was possible in this environment (no browser-automation tool available and the
  Go backend is out of scope/owned by a concurrent agent) — verification stopped at clean build + type-check + tests
  + same-origin asset resolution via static serving.

## Found but out of scope (belongs in `Apps.vue`, not touched)

Not inspected in detail since editing it is off-limits, but worth flagging for the owning agent: `Apps.vue` almost
certainly has the same class of icon-button/table-row-action touch-target gaps as `DeploymentList.vue`/
`DomainsPanel.vue` did, given it shares the same `UButton size="sm"/"xs" square` patterns visible from its imports
in the other files. Recommend the same `.tap44`/`.tap-row` utilities added to `main.css` in this change — they're
already available for reuse there, no need to reinvent them.

## Concerns

- The `.tap44` overlay technique intentionally allows adjacent hit areas to overlap slightly when two small controls
  sit close together on desktop's dense spacing (e.g., two 28px icon buttons 6px apart both get 44px hit zones,
  which overlap by design). `.tap-row` widens the gap under coarse pointer specifically to reduce this, but for the
  very tightest clusters (e.g., DeploymentList's per-row actions) a mis-tap landing on the neighboring control is
  still possible, just less likely than before. This is the standard, accepted trade-off for retrofitting touch
  targets onto an already-dense desktop layout without a visual redesign — flagging it rather than hiding it.
- I did not touch `UAccordion` in `GitPanel.vue` (the GitHub/GitLab/Gitea provider-hint disclosure) — confirmed by
  reading its actual compiled theme (`py-3.5` trigger padding + icon/line-height) that it already renders at ~48px,
  so no fix was needed there.
