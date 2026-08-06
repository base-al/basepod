// Detects NewApp.vue's upload-source placeholder image
// ("localhost/basepod/<slug>:pending" — see submitUpload in
// pages/NewApp.vue), the image an app is created with just before its
// first tarball deploy sets the real one. If that first deploy never
// lands (upload abandoned, failed, tab closed mid-upload), the app is
// left stuck on this placeholder forever — AppDetail.vue uses this
// predicate to detect that state and steer the user away from a plain
// "Deploy" (which would just try, and fail, to pull a tag that was never
// pushed anywhere) and ImageRef.vue uses it to avoid showing the raw
// meaningless tag.
//
// Pulled into its own module (rather than inlined as a computed) so the
// exact-match semantics are unit-tested directly, independent of Vue.

const PENDING_IMAGE_PREFIX = 'localhost/basepod/'
const PENDING_IMAGE_SUFFIX = ':pending'

/** True only for an exact "localhost/basepod/<non-empty-slug>:pending"
 * shape — NOT for any image that merely ends in ":pending" (a user's own
 * registry could legitimately have an image tagged that way) or merely
 * starts with "localhost/basepod/" (a real build's image, once deployed,
 * also lives under that prefix — see NewApp.vue's comment on
 * placeholderImage — but with a real tag, not literally "pending"). */
export function isPendingPlaceholder(image: string): boolean {
  if (!image.startsWith(PENDING_IMAGE_PREFIX) || !image.endsWith(PENDING_IMAGE_SUFFIX)) {
    return false
  }
  const slug = image.slice(PENDING_IMAGE_PREFIX.length, image.length - PENDING_IMAGE_SUFFIX.length)
  return slug.length > 0
}
