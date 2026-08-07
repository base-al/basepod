# Users + Roles (RBAC, no teams) — implementation report

## Status

Complete. Branch `feat/users-roles`, pushed to `origin/feat/users-roles`.
Local verification green (`go build ./...`, `go vet ./...`, `gofmt -l .`
clean, `go test ./... -count=1` green, `go test ./internal/api/ -race
-count=1` green, ~301s). CI status: see below (background-watched at
report time).

## Commit

- `e45e287e62a2f9b807fb412b39cb2192de3f7136` —
  `feat(auth,api,store,cli): add users, roles, and RBAC (no teams)`
  (single commit, 19 files changed)

## Capability → role table, as implemented

Source of truth: `internal/authz/authz.go`'s `minRole` map, exhaustively
tested by `internal/authz/authz_test.go`'s `TestCapabilityMatrix`
(every `Capability` × every role).

| Capability | viewer | member | admin | owner | Route(s) |
|---|---|---|---|---|---|
| `apps:read` | ✓ | ✓ | ✓ | ✓ | GET /apps, /apps/{slug}, /apps/{slug}/deployments/{n}, /apps/{slug}/volumes |
| `env:read` | ✓ | ✓ | ✓ | ✓ | GET /apps/{slug}/env |
| `domains:read` | ✓ | ✓ | ✓ | ✓ | GET /apps/{slug}/domains |
| `git:read` | ✓ | ✓ | ✓ | ✓ | GET /apps/{slug}/git, /apps/{slug}/git/deliveries |
| `logs:read` | ✓ | ✓ | ✓ | ✓ | GET /apps/{slug}/logs (SSE), /apps/{slug}/deployments/{n}/log (SSE) |
| `stats:read` | ✓ | ✓ | ✓ | ✓ | GET /apps/{slug}/stats (SSE), /stats (SSE) |
| `system:read` | ✓ | ✓ | ✓ | ✓ | GET /system |
| `apps:write` | | ✓ | ✓ | ✓ | POST /apps, PATCH /apps/{slug}, DELETE /apps/{slug} |
| `apps:deploy` | | ✓ | ✓ | ✓ | POST .../deploy, .../deploy/tarball, .../deploy/git, .../rollback, POST /compose/up |
| `env:write` | | ✓ | ✓ | ✓ | PUT /apps/{slug}/env |
| `domains:write` | | ✓ | ✓ | ✓ | POST/DELETE /apps/{slug}/domains[/{id}] |
| `git:write` | | ✓ | ✓ | ✓ | PUT/DELETE /apps/{slug}/git, POST .../git/rotate-secret |
| `users:read` | | | ✓ | ✓ | GET /users |
| `users:invite` | | | ✓ | ✓ | POST /users/invite |
| `users:disable` | | | ✓ | ✓ | POST /users/{email}/disable, /enable |
| `registries:manage` | | | ✓ | ✓ | *(no route yet — reserved; no registries feature exists in this codebase)* |
| `instance:settings` | | | ✓ | ✓ | *(no route yet — reserved; no instance-settings API exists)* |
| `exec` | | | ✓ | ✓ | *(no route yet — reserved for container exec/terminal)* |
| `audit:read` | | | ✓ | ✓ | GET /audit |
| `users:role_change` | | | | ✓ | PATCH /users/{email}/role |
| `users:remove` | | | | ✓ | DELETE /users/{email} |

Self-scoped routes (`GET /auth/me`, `POST /auth/logout`, `GET/DELETE
/auth/sessions[/{id}]`, `POST /auth/password`, `POST /stream-token`) are
**not** capability-gated — they act on the caller's own account/access
with no role distinction, so `requireAuth` alone is correct (equivalent
to a viewer floor, since there's no role below viewer to exclude).

**Interpretation calls I made** (task text was ambiguous on a few
points; documented here rather than silently decided):
- "admin: manage users and invitations" → I split this into
  `users:read`/`users:invite`/`users:disable` (admin), while
  `users:role_change` and `users:remove` are owner-only, per the task's
  explicit "owner: + change another user's role, remove users" bullet.
  So an admin can invite/list/disable/enable users but not change roles
  or delete accounts.
- `authz.CanAssignRole(actor, target)`: an inviter/role-changer may only
  grant a role at or below their own rank — otherwise an admin could
  mint themselves a fresh owner account via invite, defeating
  "role changes are owner-only". Enforced in both `handleInviteUser` and
  `handleChangeUserRole`; the latter is already owner-gated so this is
  redundant there but kept for symmetry/defense in depth.
- Viewer's env access: the task says "read apps/deployments/domains/env
  **keys**" for viewer. I kept `GET /apps/{slug}/env`'s existing response
  shape (keys + non-secret values, secrets always masked) at the viewer
  floor rather than stripping non-secret values for viewer specifically
  — that principle ("secrets stay masked for everyone") already held
  pre-existing and I didn't want to regress or complicate it further by
  making the response role-dependent. Flagged here in case the intent
  was stricter.

## Named test results

All PASS, `go test ./... -count=1`:

- **Capability matrix** (the specification): `internal/authz`
  `TestCapabilityMatrix` — PASS. Verified as a real drift detector
  during development (temporarily changed `CapUsersRemove`'s minRole
  from owner to admin — test failed with a clear diff; reverted).
  `TestEveryCapabilityIsInTheMatrix` — PASS (keeps the matrix
  exhaustive against `authz.go`'s constants).
- **Route-level 403 wiring**: `internal/api` `TestRouteCapabilityMatrix`
  — PASS (table-driven: createApp, deleteApp, listUsers, inviteUser,
  disableUser, listAudit, changeUserRole — each asserts 403 for an
  under-privileged role and non-403 for a sufficient one, over real
  HTTP through the real router). `TestViewerCanReadButNotWrite` — PASS.
- **Owner protection**: `internal/store`
  `TestSetUserRoleLastOwnerProtection`,
  `TestSetUserRoleLastOwnerProtectionIgnoresDisabledOwners`,
  `TestDisableUserLastOwnerProtection`,
  `TestDeleteUserLastOwnerProtection` — all PASS. HTTP-level:
  `internal/api` `TestLastOwnerCannotBeDemoted`,
  `TestLastOwnerCannotBeDisabled`, `TestLastOwnerCannotBeDeleted`,
  `TestNonLastOwnerCanBeDemotedDisabledDeleted` — all PASS.
- **Session revocation on disable/delete**: `internal/store`
  `TestDisableUserRevokesSessions`, `TestDeleteUserCascadesSessions` —
  PASS. HTTP-level: `internal/api`
  `TestDisabledUserCannotAuthenticate` (old token 401s immediately,
  disabled user also can't re-login), `TestDeletedUserSessionRevoked` —
  PASS.
- **Invitations**: `internal/store` `TestInvitationLifecycle`,
  `TestInvitationByTokenHashNotFound` — PASS. HTTP-level:
  `internal/api` `TestInviteAndAcceptFlow` (single-use: replay of the
  same token → 409 `invite_already_used`), `TestAcceptInviteUnknownToken`,
  `TestAcceptExpiredInvite`, `TestInviteRoleAssignmentBoundedByActorRank`
  — all PASS.
- **Secrets stay masked for every role**: `internal/api`
  `TestSecretEnvValuesMaskedForEveryRole` (owner/admin/member/viewer,
  same secret key, all see `""`) — PASS.
- **Bootstrap from v0.5**: `internal/store`
  `TestBootstrapFromV05YieldsOneOwnerNoLockout` — PASS (drives goose to
  version 9, inserts a user the pre-role-column way, migrates to
  latest, asserts exactly one user, role `owner`, not disabled, and
  that `SetUserRole` refuses to demote them as the last owner).
- **Audit log**: `internal/store` `TestAuditLog`, `TestAuditLogLimit` —
  PASS. HTTP-level: `internal/api`
  `TestAuditLogRecordsLoginAndAppCreate` — PASS.
- **OpenAPI conformance**: `internal/api` `TestOpenAPIConformance` —
  PASS (every new route documented, no drift either direction).
- **CLI**: `internal/cli` `TestUsersListRendersTable`,
  `TestUsersInvitePrintsTokenOnce`, `TestUsersInviteRequiresRoleFlag`,
  `TestUsersRolePatchesCorrectPath`, `TestUsersDisableAndEnable`,
  `TestUsersRemoveDeletesCorrectPath`,
  `TestUsersRoleSurfacesLastOwnerError` — all PASS.

Full suite: `go test ./... -count=1` → all packages `ok`.
Race: `go test ./internal/api/ -race -count=1` → `ok` (~301s).
`go build ./...`, `go vet ./...` clean. `gofmt -l .` clean (excluding
pre-existing `web/` files, which I did not touch).

## CI conclusion

Pushed to `origin/feat/users-roles`; CI run
`https://github.com/base-al/basepod/actions/runs/31177555682` launched
immediately after push and was still running as this report was
written (watched via `gh run watch` in the background — local
verification above already covers everything CI runs). Check
`gh run list --branch feat/users-roles` for the final conclusion before
merging if it isn't visible yet.

## What the web agent needs (exact endpoint shapes)

All under `/api/v1`, bearer session auth unless noted.

**`GET /auth/me`** (existing route, now richer) →
```json
{"id": 1, "email": "a@b.c", "name": "Admin", "role": "owner"}
```
`POST /auth/login` and `POST /invitations/accept` return the same
`user` shape nested in `{"token": "...", "user": {...}}`.

**`GET /users`** → array of:
```json
{"id": 1, "email": "a@b.c", "name": "Admin", "role": "owner", "disabled": false, "created_at": "2026-08-07T12:00:00Z"}
```

**`POST /users/invite`** body `{"email": "...", "role": "viewer|member|admin|owner"}` →
```json
{"email": "...", "role": "...", "token": "bp_invite_...", "expires_at": "..."}
```
`token` is shown **once** — the dashboard is responsible for building
whatever accept-invite URL it wants around it (the API has no notion of
the dashboard's own public URL). Errors: 409 `user_exists`, 422
`validation`, 403 (inviting a role above the caller's own rank).

**`POST /invitations/accept`** — unauthenticated. Body
`{"token": "...", "name": "...", "password": "..."}` → same shape as
login (`{"token", "user"}"`), 201. Errors: 404 `invite_not_found`, 409
`invite_already_used` / `invite_expired` / `user_exists`, 422
`validation`.

**`PATCH /users/{email}/role`** (owner only) body `{"role": "..."}` →
updated `UserSummary`. 409 `last_owner` if this is the last owner.

**`POST /users/{email}/disable`** / **`POST /users/{email}/enable`**
(admin) → updated `UserSummary`. Disable 409s `last_owner` if it's the
last owner; disabling kills that user's sessions immediately.

**`DELETE /users/{email}`** (owner) → 204. 409 `last_owner` if it's the
last owner.

**`GET /audit?limit=100`** (admin) → array of:
```json
{"id": 1, "actor_email": "a@b.c", "action": "app.deploy", "target": "myapp", "detail": "image=nginx:latest", "created_at": "..."}
```

Every one of the above is in `api/openapi.yaml` under the new `users`
and `audit` tags, with request/response schemas and error codes — that
file is the authoritative shape reference and is kept in sync by
`TestOpenAPIConformance`.

**403 vs 401**: a capability-gated route now returns `403
{"error":{"code":"forbidden","message":"..."}}` for an authenticated
but under-privileged caller — the dashboard should treat this as "hide/
disable the action", not as a logout trigger the way 401 is.

## Concerns / follow-ups for the user

1. **Interpretation calls above** (admin vs owner split on user
   management, `CanAssignRole`, viewer env-keys-vs-values) were my best
   reading of an ambiguous brief — worth a quick confirm.
2. **No revoke-invitation endpoint.** The store has everything needed
   (`ListInvitations`, and a delete could be added trivially), but the
   task's explicit endpoint list didn't include it and I kept scope to
   what was asked. A stale/unwanted pending invite can't currently be
   canceled — only left to expire (7 days).
3. **`registries:manage` / `instance:settings` / `exec` capabilities
   exist in the table with no backing routes** — those features don't
   exist in this codebase yet. Wiring them up is a one-line
   `requireCapability` addition whenever those routes land; no schema
   or authz changes needed.
4. **Audit coverage is the task's explicit list** (deploys, app
   create/delete, env key changes, user/role changes, logins) — I did
   not extend it to domains/git/compose-apply mutations, which are also
   "mutating operations" in spirit but weren't named. Easy to add later
   the same way (`a.audit(r, "domain.add", ...)` etc.) if wanted.
5. **CLI adds `basepod users enable`** beyond the task's literal
   `list|invite|role|disable|remove` set — without it there'd be no CLI
   path back from an accidental disable short of calling the API
   directly. Flagged in case that's unwanted scope creep.
6. I did not touch `docs/plan/08.teams-and-rbac.md`'s "Status: not
   implemented" banner or checklist — left as-is since the task didn't
   ask for a docs update and that file describes the (broader,
   team-scoped) future design this milestone deliberately diverges
   from.
