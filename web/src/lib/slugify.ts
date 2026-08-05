// Client-side mirror of internal/api/apps.go's slugify(): lowercase,
// spaces become hyphens. Pulled out of NewApp.vue so it's unit-testable
// without mounting Vue.
//
// This is a preview only — it does not strip punctuation beyond spaces,
// matching the Go function exactly. The result still needs validating
// against a slug pattern (see NewApp.vue's SLUG_PATTERN) before it's
// known to be a legal slug; this function alone can produce a string
// that pattern rejects (e.g. leading digit, or characters neither strip
// step removes).
export function slugify(name: string): string {
  return name.toLowerCase().replaceAll(' ', '-')
}
