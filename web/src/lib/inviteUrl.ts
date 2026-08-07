// Builds the link an invited user actually clicks, from POST
// /users/invite's plaintext `token` (see lib/api.ts's InviteUserResponse
// — shown once, never re-fetchable). The API itself has no notion of a
// "web app origin", so this is entirely a client-side construction:
// /accept-invite (router.ts's public route) plus the token as a query
// param, resolved against wherever this dashboard is actually being
// served from. Pulled out of Users.vue so the URL shape is unit-testable
// without mounting Vue, and so there's exactly one place that shape is
// defined — AcceptInvite.vue reads the same `token` query param this
// writes.

/** `origin` is passed in (rather than read from `window.location.origin`
 * internally) so this stays a pure function callers can unit-test with
 * an arbitrary origin, and so it never silently does the wrong thing if
 * this dashboard is ever served from something other than `window` (a
 * preview/SSR context). Every real call site passes
 * `window.location.origin`. */
export function buildInviteAcceptUrl(origin: string, token: string): string {
  const url = new URL('/accept-invite', origin)
  url.searchParams.set('token', token)
  return url.toString()
}
