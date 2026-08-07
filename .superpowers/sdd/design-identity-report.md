# BasePod dashboard — design identity report

## Status

Complete. `npm run build`, `npm run type-check`, and `npm test` all pass clean (68/68 tests, unchanged assertions). `go build ./cmd/basepod` succeeds with the new dashboard embedded. Brandbook published at `site/public/brand/index.html`.

Direction followed the user-approved reference at `docs/design/identity-concept.html` (commit `505c382`), pulled into this branch mid-work — an earlier pass of mine (before that commit existed) had built an original palette/mark from scratch; it was fully replaced once the approved concept landed. The real wordmark (`web/public/wordmark.svg`, commit `19a08ca`) is adopted as-is, per instruction, and is the only asset taken from the source project — no other palette, component, or decision was carried over from it.

## Commits

Not yet committed — see the end of this report for the exact `git add`/`git commit` this work is staged for. (Committing was deferred until this report was written, per the task's own ordering: verify, then report, then commit.)

## Identity decisions

**Palette — copper on warm graphite.** Five hue families, all independently measured for contrast (not eyeballed):
- **ink** (ground/raised/sunken/ink/ink-2/ink-3) — a neutral ramp with a deliberate *warm* bias (hue ~25), not the cool blue-gray of a generic "terminal" self-hosting tool.
- **copper** (hue ~21) — the brand accent. Metal: hardware you own, in a rack you chose. Kept out of the status-color hue family entirely so a primary button never reads as a health signal.
- **moss** (steady/running, hue ~155), **ember** (moving/deploying, hue ~40), **crimson** (broken/error, hue ~345).

**Mark.** The wordmark is "basepod" in rounded, lowercase, geometric letterforms with a tucked descender on the `p` — a single SVG path from the user's other project, used unmodified via `currentColor` (`BasepodWordmark.vue`). The compact mark (`BasepodMark.vue`) is that same path's own **b** — the exact outer-contour and counter subpaths that draw it in the full word, not a fresh drawing — with an *optically* (not mathematically) balanced crop: a uniform bounding-box pad left the glyph looking bottom-heavy and pushed left (the bowl doesn't start until roughly a third of the way down, and its curves read lighter than the stem's hard edges), so the final viewBox (`-5 -8 83 113`, vs. a naive `-6 -6 83 113`) trims tighter top/left and gives the bowl more room bottom/right. Both marks accept a `tone` prop that tints via `currentColor` — brand copper by default, or a status color (used on the mobile header's compact mark when the health chip itself doesn't fit, so the mark doubles as the indicator).

**Type — mono-led.** JetBrains Mono for headings, section labels, and every technical value (slugs, image refs, hostnames, ports, deployment numbers — numerics get `tabular-nums`). Running prose stays in the system-sans stack, which needs no webfont. This is a deliberate simplification from my own first pass (which self-hosted a second display face, Space Grotesk) — the approved concept needs exactly one font family, not two, and specifies system sans is sufficient for prose. JetBrains Mono is self-hosted (OFL-1.1, bundled by Vite, served same-origin) because the dashboard's `default-src 'self'` CSP silently blocks a CDN font.

**Layout — a rail, not a shrinking header.** Desktop/tablet gets a fixed left rail (brand, nav, health/version, theme toggle, logout). Phone gets two purpose-built pieces instead: a compact sticky top bar (mark + health + quick actions) and a fixed bottom tab bar for primary navigation — not the rail with a media query. See "Mobile" below for the full per-screen breakdown.

## Contrast ratios (measured, WCAG relative-luminance formula)

Several of the approved concept's exact hex values fell short of 4.5:1 for their actual use as text; each was nudged in lightness only (same hue/saturation) until it cleared the bar — the token moved, not the rule.

| Token | Dark | Ratio | Light | Ratio |
|---|---|---|---|---|
| content-primary | `#edeae5` | **15.45:1** | `#1b1a18` | **15.58:1** |
| content-secondary | `#a8a39b` | **7.40:1** | `#4a463f` | **8.40:1** |
| content-muted | `#8b8179` (was `#757069`, 3.78:1) | **4.87:1** | `#796c63` (was `#7a736a`, 4.19:1) | **4.55:1** |
| accent, as text | `#e2743c` | **6.00:1** | `#ab4e1c` (was `#b4531f`, 4.48:1) | **4.90:1** |
| accent-fg on solid button | `#131316` on `#e2743c` (white only hit 3.09:1) | **6.00:1** | white on `#b4531f` | **5.00:1** |
| status-running | `#45b08f` | **6.94:1** | `#1f6f5c` | **5.40:1** |
| status-deploying | `#d9a23b` | **8.11:1** | `#92650c` (was `#9a6b10`, 4.20:1) | **4.60:1** |
| status-error | `#f0567b` | **5.58:1** | `#b32544` | **5.76:1** |
| line-strong (input/button borders, non-text 3:1 target) | `#6e655e` | 3.26:1 | `#95887e` | 3.08:1 |
| line (decorative dividers only — not held to a minimum) | `#2c2c33` | 1.34:1 | `#d9d2c9` | 1.34:1 |

All status dots (small colored circles) are always paired with a text label (StatusBadge, connection chips) — the label carries the meaning, so the bare dot color isn't held to the same minimum. `prefers-reduced-motion: reduce` is respected globally (collapses all transitions/animations, including the deploying-status pulse).

## Font/icon bundling verification

- Built `web/dist/assets/index-*.css` and grepped for `url(...)`: every `@font-face` src is `/assets/jetbrains-mono-*.woff2|woff` — root-relative, same-origin, zero CDN references.
- Grepped the whole `dist/assets/*.{js,css}` tree for `https?://` hosts: only `github.com` (a pre-existing hardcoded link, not mine), `tailwindcss.com`/`vuejs.org` (license/error-message strings baked into the libraries, not fetched), and `api.iconify.design`/`api.simplesvg.com`/`api.unisvg.com` (dead-code fallback paths inside `@iconify/vue` itself, unreachable because `clientBundle.scan: true` — already configured before this pass — pre-bundles every icon actually used, which is exactly what makes those fallback URLs unreachable).
- Served `web/dist/` with a plain static file server and confirmed `/`, `/favicon.svg`, `/wordmark.svg`, the JS/CSS bundles, and a font file all return 200 from `localhost` — nothing 404s, nothing points off-origin.
- **New icon introduced:** only one, `i-lucide-settings` (desktop rail + mobile bottom nav). Verified its exact SVG path geometry (`M9.671 4.136a2.34...`) is present in the built `dist/assets/index-*.js` chunk via direct grep — it's bundled, not fetched from `api.iconify.design` at runtime. Every other icon used was already present before this pass.
- `go build -o /tmp/bp ./cmd/basepod` succeeds with the new dashboard embedded (confirms the Go embed still finds a valid `dist/`). I did **not** start the binary as a server — per the brief, an earlier agent's cleanup against this machine's real Podman deleted a live container, so verification stayed at static-serve + build-succeeds rather than a live instance.
- `web/dist/index.html` was reset back to the committed placeholder (`git checkout -- web/dist/index.html`) after every local build — the repo's own convention (`web/embed.go`'s doc comment) is that only the placeholder is committed; the real build must never be.

## Test results

- `npm run build` — clean.
- `npm run type-check` (`vue-tsc --noEmit`) — clean.
- `npm test` (vitest) — **68/68 passing, 9/9 files**, no assertions changed. These tests cover pure logic (`envparse`, `gitFormat`, `slugify`, `relativeTime`, `version`, `composePreview`, `formatLimits`, `pendingImage`, `sse`) — none render components, so a visual reskin had nothing to break there by construction, and nothing was touched to make them pass.

## Screens, described

Both themes are driven by the `.dark`/`.light` class already on `<html>` (existing `useColorMode` composable, untouched); dark is default. Below, "desktop" means ≥768px (the rail breakpoint); "phone" means <768px, checked in particular against 390px.

### AppShell (chrome around every authenticated page)

**Desktop.** A 224px sunken left rail spans full height: the wordmark near the top, then Apps / Settings as a vertical list (active item gets a 2px copper left-border and a faint copper-tinted background, inactive items are secondary-gray text), then — pinned to the bottom — a small mono health line (a dot, `podman: ok`/version, or `unreachable` in error-red) and a compact theme-toggle + Logout row. The main content area fills the rest of the viewport with its own scroll.

**Phone (390px).** The rail is gone. A 56px sticky top bar holds the compact "b" mark + `basepod` wordmark-text in mono, a small health dot, and icon-only theme-toggle/logout buttons (both keep `aria-label`s — they're actions, not navigation, so the "icon-only nav" rule doesn't apply to them). A fixed bottom bar holds Apps and Settings, each a full-height (56px+, safe-area-padded) tap target with an icon **and** visible text label — never icon-alone. Main content gets bottom padding so the last row of any page isn't hidden under the fixed bar.

### Login

**Desktop/phone, both themes.** Centered card on a faint dot-grid background (a schematic/blueprint texture, built from the `line` token at low opacity — a small nod to "precision instrument" a generic template wouldn't bother with). Above the card: the full wordmark and the line "Your server. Your data. No vendor." Below: email/password fields, an inline error alert when login fails, and a full-width Sign-in button. Nothing here needed phone-specific changes — a single centered card already collapses cleanly.

### Apps (the list)

**Desktop.** A mono page title ("Apps"), app count, and a "New app" button. Below: either a loading skeleton, an empty state (rocket icon, plain-language copy), or the app list — grouped by compose project when any exist, otherwise one flat list. Each group renders as a bordered table: App (mono slug) / Status (dot + word) / Image (middle-truncated mono) / Port (mono, tabular) / Limits (mono, tabular). The whole row is a link; hover/focus both get a background tint and a visible focus ring.

**Phone (390px).** The table is replaced (a CSS breakpoint swap, not a squeeze) by a stack of cards — same fields, reflowed: slug + status on one line, the image ref on its own line, port and limits on a final line. No horizontal scroll anywhere on this page.

### AppDetail

**Desktop.** Header: back-link, mono app slug, status badge, the generated domain as a copper link (opens in a new tab), and the image ref pushed right. Below: a tab strip (Overview / Deployments / Logs / Environment / Domains / Git / Settings) as a single row of links with an active underline. Overview shows a Facts card and a Quick Actions card (Deploy / Deploy new image / Delete), both with mono section labels.

**Phone (390px).** The tab strip now scrolls horizontally as one row instead of wrapping into a multi-line block that would push content off-screen — I overrode Nuxt UI's `UTabs` `list` slot to `flex-nowrap overflow-x-auto` (scrollbar hidden, but still keyboard/swipe reachable) so all seven tabs stay one swipe away and the active tab is never hidden behind a "more" menu. Header fields wrap naturally (`flex-wrap`, already in place) rather than truncating unreadably.

### LogViewer / BuildLogPanel

Both already wrap long lines (`whitespace-pre-wrap break-all`) rather than scrolling horizontally — confirmed as the deliberate choice for phone rather than something I needed to add: a horizontally-scrolling log pane fights the page's own scroll and a thumb's natural swipe direction, whereas wrapped lines stay readable and the log box's own `overflow-y-auto` keeps its scroll contained (the page body itself never scrolls sideways). The follow/pause/tail controls sit in a `flex-wrap` bar directly above the log box — near the top of the tab's content, not buried below hundreds of lines — so they stay reachable with a thumb without a long scroll, and they wrap onto a second row at 390px rather than overflowing.

### EnvEditor

Each row (key / value-or-masked-secret / secret-switch / delete) is a `flex flex-wrap` group where the key and value inputs are `w-full` at the base breakpoint and only get fixed/flex widths at `sm:` — meaning it was **already** stacking correctly on phone (key on its own line, value on its own line, switch+delete on a third) before this pass; I verified this rather than needing to rebuild it, since it already met the "not three cramped columns" requirement.

## Concerns

- **`docs/design/identity-concept.html`'s console mock (Apps-as-operator-console with per-app CPU/memory sparklines) was not built.** That section of the mock shows live stats (CPU%, memory, a sparkline) on the apps *list* page. I could not confirm the `GET /apps` list endpoint returns that data (the app list's own code comments explicitly note it returns `App[]`, not per-app stats, and fetching stats per row would be an N+1 the existing codebase deliberately avoids elsewhere). Rather than invent a field or wire up N+1 requests, I kept the list's real, existing fields (status/image/port/limits) restyled to the new system, and I'm reporting this gap instead of fabricating data. If live per-app stats belong on the list view, that's a real API surface to add first.
- **`--ui-primary` (and its `--ui-success`/`-warning`/`-error` siblings) are set by `@nuxt/ui`'s Vue-plugin at runtime, not baked into the static CSS build** — I traced this as far as static analysis allows (confirmed the mechanism is CSS-custom-property-based, confirmed `unplugin.mjs`'s only `tailwindcss/colors` import is an unused bare import, so my non-stock color names — copper/moss/ember/crimson — aren't at risk of a JS module lookup miss) but could not do a pixel-level runtime confirmation without launching a browser against a live server, which the brief asked me to avoid. As a defensive measure I filled out the **full 50–950 ramp** for all four custom colors (not just the two AA-verified steps each actually uses), so that whichever shade Nuxt UI's runtime picks, something reasonable is defined rather than nothing.
- **Touch targets:** the bottom nav (56px) and top-bar icon buttons meet the 44px guideline; several inline `size="sm"` `UButton`s inside dense areas (EnvEditor row actions, DeploymentList row actions) are Nuxt UI's own smaller default and were **not** widened app-wide — doing so risked breaking the intentionally dense desktop density for a component library default I don't fully control the internals of. Flagging rather than silently leaving unmentioned.
- **The marketing site's own existing pages** (`site/public/index.html`, `style.css`) were **not** reskinned — only `site/public/brand/index.html` (new) and `site/public/wordmark.svg`/`fonts/` (added) were touched. The task scoped `site/` involvement to the brandbook; the rest of the marketing site still carries its older slate/emerald identity.
- I do not own Go and touched none of it; nothing here required an API field that doesn't already exist, except the stats/sparkline gap noted above.

## Files touched

`web/**` (theme, AppShell, all pages, LogViewer/BuildLogPanel/EnvEditor/GitPanel/ComposePreview/DeploymentList restyled to tokens, `BasepodWordmark.vue`/`BasepodMark.vue` new, `favicon.svg` regenerated, `wordmark.svg` added), `site/public/brand/index.html` (new), `site/public/fonts/*.woff2` (new, self-hosted JetBrains Mono for the brandbook), `site/public/wordmark.svg` (added), `docs/design/identity-concept.html` (the approved reference, pulled in from origin/main).
