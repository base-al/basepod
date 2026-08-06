// Webhook receiver for git push-to-deploy (v0.5 plan Task 5) — the ONLY
// unauthenticated route in BasePod's API, so it is treated as hostile
// input throughout this file:
//
//   - The URL carries only an opaque, random hook_id (see git.go's
//     newHookID) — never the secret itself.
//   - Signature verification is constant-time (hmac.Equal /
//     subtle.ConstantTimeCompare, never ==) and happens before any parsing
//     of the body beyond the byte-cap read.
//   - An unknown hook_id and a known hook's bad/missing signature return
//     the exact same response (status, body, error code) — see
//     handleGitWebhook's doc comment for why, and dummyWebhookSecret for
//     how the same signature-verification work runs either way.
//   - The body is capped well below the JSON routes' own 1 MiB
//     maxJSONBody-via-bodyLimit cap (this route isn't behind that
//     middleware at all — see router.go — and enforces webhookMaxBody
//     itself).
//   - A push to a non-configured branch is accepted and recorded, but
//     never deploys.
//   - A flood of pushes is rate-limited per hook_id AND coalesced per app
//     (see git.go's gitCoalescer) so it can never spawn unbounded clones.
//   - The payload can trigger a deploy and filter which branch triggers
//     one — it can NEVER steer what gets cloned: the URL, branch, and
//     credentials always come from the stored git_sources row, never from
//     the request body (see triggerGitDeploy in git.go).
//   - Every delivery — accepted, ignored, or rejected — is recorded for
//     the dashboard, with a short human-readable reason; the payload body
//     and the secret/token are never logged or stored.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/base-al/basepod/internal/store"
)

// webhookMaxBody caps a webhook payload well below maxJSONBody (the
// authenticated JSON routes' own cap): real push payloads from GitHub/
// GitLab/Gitea are at most a few hundred KB even for a busy push, so 1
// MiB is already generous headroom, not a limit tuned to actual traffic.
const webhookMaxBody = 1 << 20 // 1 MiB

// dummyWebhookSecret stands in for a real secret when hook_id doesn't
// resolve to any git source, so verifyWebhookSignature still does the
// same HMAC/constant-time-compare work it would for a known hook — see
// handleGitWebhook's doc comment for why an unknown hook_id and a known
// hook's bad signature must not be distinguishable to the caller. It is
// never valid: no real secret is ever this fixed string (see
// newGitSecret — a real secret is 16 random bytes, and this "value" is
// visibly not that).
const dummyWebhookSecret = "0000000000000000000000000000000000"

// branchRefPrefix is the "refs/heads/" prefix a push payload's ref carries
// for an ordinary branch push (as opposed to a tag push, "refs/tags/...",
// which never matches a configured branch and is treated the same as any
// other non-matching ref — see handleGitWebhook).
const branchRefPrefix = "refs/heads/"

// zeroSHA is the all-zeros commit git/forges use in a push payload's
// "after"/"checkout_sha" to signal a branch deletion (there is no new
// HEAD to build).
const zeroSHA = "0000000000000000000000000000000000000000"

// handleGitWebhook receives a push notification from GitHub, GitLab, or
// Gitea for exactly one app (resolved via hook_id) and, if it verifies
// and names the app's configured branch, triggers a deploy of that app's
// stored repo config — never anything the payload itself names (see this
// file's package doc comment).
//
// Response codes: 401 "unauthorized" for BOTH an unknown hook_id and a
// known hook's invalid/missing signature (deliberately identical — an
// attacker probing hook_ids must not be able to tell "this one doesn't
// exist" from "this one exists but I don't have its secret", which a
// distinguishing 404-vs-401 split would hand them for free); 413
// "request_too_large" over the body cap; 429 "rate_limited" per hook_id;
// 503 "git_unavailable" if git deploys are disabled; 200/202 with a
// {"status": "..."} body for every other outcome (ignored, coalesced, or
// queued) — a push is never bounced with an error status just because
// BasePod chose not to act on it.
func (a *api) handleGitWebhook(w http.ResponseWriter, r *http.Request) {
	hookID := chi.URLParam(r, "hookID")

	gs, lookupErr := a.st.GitSourceByHookID(hookID)
	known := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		log.Printf("api: webhook: look up hook: %v", lookupErr)
		known = false
	}

	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			if known {
				a.recordGitDelivery(gs.AppID, gitDeliveryInput{Status: "error", Detail: "payload exceeds the webhook size cap"})
			}
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "webhook payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}

	// Rate limit before doing any crypto work or DB writes beyond the
	// hook_id lookup already above — keyed by hook_id (opaque,
	// unguessable), so the limit is per-configured-hook regardless of how
	// many source addresses a flood comes from, and applies even to an
	// unknown hook_id (bounded by rateLimiter's own sweep — see
	// router.go — so probing many bogus hook_ids can't grow it
	// unboundedly either).
	if !a.webhookLimiter.Allow(hookID) {
		if known {
			a.recordGitDelivery(gs.AppID, gitDeliveryInput{Status: "rate_limited", Detail: "rate limit exceeded"})
		}
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
		return
	}

	secret := dummyWebhookSecret
	var appID int64
	if known {
		if s, err := a.open(gs.AppID, "git_secret", gs.SecretEncrypted); err == nil {
			secret = s
			appID = gs.AppID
		} else {
			log.Printf("api: webhook: decrypt secret for app %d: %v", gs.AppID, err)
			known = false
		}
	}

	provider, event := webhookProviderEvent(r.Header)
	sigOK := verifyWebhookSignature(r.Header, body, secret)

	if !known || !sigOK {
		// Deliberately identical response for both branches — see this
		// handler's doc comment. Only the known branch has an app to
		// attach a delivery record to; that asymmetry is server-side
		// bookkeeping only and never reaches the response written below.
		if known {
			a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Status: "invalid_signature", Detail: "signature verification failed"})
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid signature")
		return
	}

	if event == "ping" {
		a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Status: "ignored_event", Detail: "ping"})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "ping"})
		return
	}
	if event != "push" {
		a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Status: "ignored_event", Detail: "event is not push"})
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "unsupported_event"})
		return
	}

	ref, sha, parseErr := parsePushPayload(body)
	if parseErr != nil {
		a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Status: "error", Detail: "malformed push payload"})
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed push payload")
		return
	}

	if isZeroSHA(sha) {
		a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Ref: ref, Status: "ignored_event", Detail: "branch deleted"})
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "branch_deleted"})
		return
	}

	branch, isBranch := strings.CutPrefix(ref, branchRefPrefix)
	if !isBranch || branch != gs.Branch {
		a.recordGitDelivery(appID, gitDeliveryInput{
			Provider: provider, Event: event, Ref: ref, CommitSHA: sha, Status: "ignored_branch",
			Detail: fmt.Sprintf("push to %q, configured branch is %q", ref, gs.Branch),
		})
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "branch_mismatch"})
		return
	}

	app, err := a.st.AppByID(appID)
	if err != nil {
		log.Printf("api: webhook: look up app %d: %v", appID, err)
		a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Ref: ref, CommitSHA: sha, Status: "error", Detail: "internal error"})
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if a.gitFetcher == nil {
		a.recordGitDelivery(appID, gitDeliveryInput{Provider: provider, Event: event, Ref: ref, CommitSHA: sha, Status: "error", Detail: "git unavailable"})
		writeError(w, http.StatusServiceUnavailable, "git_unavailable", "git is not available on this server")
		return
	}

	if !a.gitCoalescer.tryStart(app.ID) {
		a.gitCoalescer.setPending(app.ID, sha, ref)
		a.recordGitDelivery(appID, gitDeliveryInput{
			Provider: provider, Event: event, Ref: ref, CommitSHA: sha, Status: "coalesced",
			Detail: "a build for this app is already in progress; this commit is queued as the next build",
		})
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "coalesced"})
		return
	}

	// triggerGitDeploy resolves the URL/branch/token to clone EXCLUSIVELY
	// from gs (the stored config looked up by hook_id above) — sha/ref
	// from the payload are used only to decide *whether* to deploy
	// (already done, above) and to label the delivery/deployment records;
	// they never reach the clone itself. See this file's package doc
	// comment and the hostile-payload test in webhook_test.go.
	a.triggerGitDeploy(r.Context(), app, gs, sha, provider, event, ref, "webhook")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// webhookProviderEvent extracts the (provider, event) pair from whichever
// forge-specific event header is present: GitHub's X-GitHub-Event and
// Gitea's X-Gitea-Event both carry the event name verbatim ("push",
// "ping", ...); GitLab's X-Gitlab-Event carries a human title ("Push
// Hook") normalized to match the others ("push").
func webhookProviderEvent(h http.Header) (provider, event string) {
	switch {
	case h.Get("X-GitHub-Event") != "":
		return "github", strings.ToLower(h.Get("X-GitHub-Event"))
	case h.Get("X-Gitea-Event") != "":
		return "gitea", strings.ToLower(h.Get("X-Gitea-Event"))
	case h.Get("X-Gitlab-Event") != "":
		return "gitlab", normalizeGitLabEvent(h.Get("X-Gitlab-Event"))
	default:
		return "", ""
	}
}

// normalizeGitLabEvent maps GitLab's human-readable X-Gitlab-Event header
// values ("Push Hook", "Tag Push Hook", ...) onto the same lowercase,
// underscore-joined event names GitHub/Gitea use natively, so
// handleGitWebhook's event switch doesn't need a GitLab-specific branch.
func normalizeGitLabEvent(v string) string {
	v = strings.TrimSuffix(v, " Hook")
	v = strings.ToLower(strings.ReplaceAll(v, " ", "_"))
	return v
}

// verifyWebhookSignature checks body against whichever signature/token
// header the request actually carries, using the given secret. Every
// comparison is constant-time (hmac.Equal or subtle.ConstantTimeCompare —
// never ==, which would leak how many leading bytes matched via early
// return timing): GitHub's X-Hub-Signature-256 and Gitea's
// X-Gitea-Signature are both an HMAC-SHA256 of the raw body, hex-encoded
// (Gitea additionally accepts GitHub's header name — "Gitea also sends
// X-Hub-Signature-256", per the v0.5 plan's Task 5, so it's checked here
// as a second, equally valid header). GitLab has no HMAC scheme; its
// X-Gitlab-Token header is the shared secret itself, compared directly.
// No recognized header present → false (mapped to the same 401 an
// invalid one gets).
func verifyWebhookSignature(h http.Header, body []byte, secret string) bool {
	if sig := h.Get("X-Hub-Signature-256"); sig != "" {
		return verifyHMACSHA256Header(sig, body, secret)
	}
	if sig := h.Get("X-Gitea-Signature"); sig != "" {
		return verifyHMACHex(sig, body, secret)
	}
	if tok := h.Get("X-Gitlab-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1
	}
	return false
}

// verifyHMACSHA256Header verifies GitHub's "sha256=<hex>" signature
// header format, delegating the actual comparison to verifyHMACHex.
func verifyHMACSHA256Header(header string, body []byte, secret string) bool {
	const prefix = "sha256="
	hexSig, ok := strings.CutPrefix(header, prefix)
	if !ok {
		return false
	}
	return verifyHMACHex(hexSig, body, secret)
}

// verifyHMACHex decodes hexSig and compares it, via hmac.Equal (constant
// time for equal-length inputs; a length mismatch — e.g. a truncated or
// overlong forged signature — is rejected by Equal's own length check
// before any byte comparison, which is not a secret-dependent timing
// leak: HMAC-SHA256 output length is public knowledge, not something
// being kept secret here), against the HMAC-SHA256 of body under secret.
// A malformed (non-hex) hexSig is rejected outright.
func verifyHMACHex(hexSig string, body []byte, secret string) bool {
	given, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(given, want)
}

// pushPayload is the subset of a GitHub/GitLab/Gitea push webhook payload
// handleGitWebhook actually parses: ref and the pushed-to head commit.
// Every other field — including repository.clone_url, which a hostile
// payload might set to point somewhere else entirely — is deliberately
// never unmarshaled: this type has no field for it, so it cannot
// accidentally leak into any decision this receiver makes (see this
// file's package doc comment and the hostile-payload test).
type pushPayload struct {
	Ref         string `json:"ref"`
	After       string `json:"after"`
	CheckoutSHA string `json:"checkout_sha"`
}

// parsePushPayload extracts (ref, head SHA) from a push event's raw JSON
// body. GitHub/Gitea both use "after"; GitLab also sends "checkout_sha"
// (and "after", but checkout_sha is GitLab's documented field for this) —
// After is preferred when both happen to be present, falling back to
// CheckoutSHA otherwise.
func parsePushPayload(body []byte) (ref, sha string, err error) {
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", "", err
	}
	sha = p.After
	if sha == "" {
		sha = p.CheckoutSHA
	}
	return p.Ref, sha, nil
}

// isZeroSHA reports whether sha is empty or the all-zeros commit a push
// payload uses to signal a branch deletion (there is no new HEAD to
// build against).
func isZeroSHA(sha string) bool {
	if sha == "" {
		return true
	}
	return sha == zeroSHA || strings.Trim(sha, "0") == ""
}
