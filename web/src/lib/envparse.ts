// .env-format text parser, shared by EnvEditor.vue's bulk-edit textarea.
// Pulled out to its own module (rather than kept inline in the
// component) purely so it's unit-testable without mounting Vue.

export interface ParsedEnvEntry {
  key: string
  value: string
}

/** Parses .env-format text: blank lines and lines whose first non-blank
 * character is '#' are ignored; every other line splits on the FIRST '='
 * (so values may themselves contain '='); both key and value are
 * trimmed (this also absorbs a trailing '\r' from CRLF line endings, so
 * no separate CRLF handling is needed). Lines with no '=' at all, or
 * with a blank key, are skipped (nothing sane to do with them). Later
 * duplicate keys within the text win over earlier ones (matching typical
 * .env tooling) while keeping the key's first-seen position in the
 * returned order. Surrounding quotes in values are left as-is — this
 * parser doesn't attempt shell-style quote stripping. */
export function parseDotEnv(text: string): ParsedEnvEntry[] {
  const out = new Map<string, string>()
  for (const rawLine of text.split('\n')) {
    const line = rawLine.trim()
    if (!line || line.startsWith('#')) continue
    const eq = line.indexOf('=')
    if (eq === -1) continue
    const key = line.slice(0, eq).trim()
    const value = line.slice(eq + 1).trim()
    if (!key) continue
    out.set(key, value)
  }
  return [...out.entries()].map(([key, value]) => ({ key, value }))
}
