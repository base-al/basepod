// Normalizes a version string for display so exactly one leading "v" is
// ever shown. The server's /system endpoint returns whatever ldflags
// baked in at build time (currently always "v0.4.1"-shaped, already
// prefixed) — but the UI was blindly prepending its own "v" on top of
// that, producing "vv0.4.1". Stripping-then-prepending (rather than just
// deleting the template literal's "v{{ }}") keeps this correct even if
// the server ever starts returning a bare "0.4.1" instead.
export function formatVersion(version: string): string {
  return `v${version.trim().replace(/^v/i, '')}`
}
