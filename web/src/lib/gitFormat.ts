// Pure formatting helpers for GitPanel.vue's connected-repo and
// deliveries views. Kept dependency-free (no Vue imports) so they're
// trivially unit-testable and reusable anywhere a delivery/commit needs
// to render the same way (e.g. a future deployment-row "git" chip).

/** Shortens a full commit SHA to the conventional 7-character form GitHub/
 * GitLab/Gitea all use for display. Returns "" for an empty input (a
 * delivery that never resolved a commit — e.g. a ping event) rather than
 * a misleading empty-string slice artifact. */
export function shortSha(sha: string): string {
  if (!sha) return ''
  return sha.slice(0, 7)
}

/** Formats a push payload's ref (e.g. "refs/heads/main",
 * "refs/tags/v1.2.3") down to the branch/tag name a human expects to
 * read in a deliveries table — mirrors internal/api/webhook.go's own
 * "refs/heads/" stripping for the branch-match check, extended to also
 * label a tag push distinctly (BasePod never deploys from a tag, but a
 * delivery row can still record one arriving and being ignored). Returns
 * "" unchanged for an empty ref. */
export function formatGitRef(ref: string): string {
  if (!ref) return ''
  const branchPrefix = 'refs/heads/'
  const tagPrefix = 'refs/tags/'
  if (ref.startsWith(branchPrefix)) return ref.slice(branchPrefix.length)
  if (ref.startsWith(tagPrefix)) return `tag: ${ref.slice(tagPrefix.length)}`
  return ref
}
