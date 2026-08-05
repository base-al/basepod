package api

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/base-al/basepod/internal/store"
)

// envKeyPattern is the shape a valid env var key must have: uppercase
// letters, digits, and underscores, starting with a letter or underscore,
// up to 64 characters — matching typical shell env var naming rules.
var envKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// redeployRequiredHeader tells the UI that a change was persisted but not
// applied to any running container: env changes don't auto-redeploy, so
// the caller must trigger one to pick them up.
const redeployRequiredHeader = "X-Basepod-Redeploy-Required"

type envVarResponse struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	IsSecret bool   `json:"is_secret"`
}

// handleGetEnv returns an app's environment variables. Secret entries
// always report value:"" — their plaintext is never sent back to the
// client once sealed.
func (a *api) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	out, err := a.envResponseFor(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list env vars")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// envResponseFor loads app's env vars and decrypts the non-secret ones
// into the wire shape; secret values are masked to "".
func (a *api) envResponseFor(appID int64) ([]envVarResponse, error) {
	vars, err := a.st.ListEnvVars(appID)
	if err != nil {
		return nil, err
	}
	out := make([]envVarResponse, 0, len(vars))
	for _, v := range vars {
		ev := envVarResponse{Key: v.Key, IsSecret: v.IsSecret}
		if !v.IsSecret {
			plain, err := a.open(v.ValueEncrypted)
			if err != nil {
				return nil, err
			}
			ev.Value = plain
		}
		out = append(out, ev)
	}
	return out, nil
}

// effectiveEnvEntry is the decrypted (key, value, is_secret) triple used
// only for detecting whether a PUT actually changed the env set — never
// serialized to the client.
type effectiveEnvEntry struct {
	Value    string
	IsSecret bool
}

// effectiveEnv loads and fully decrypts appID's env vars (including
// secrets) for internal before/after comparison.
func (a *api) effectiveEnv(appID int64) (map[string]effectiveEnvEntry, error) {
	vars, err := a.st.ListEnvVars(appID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]effectiveEnvEntry, len(vars))
	for _, v := range vars {
		plain, err := a.open(v.ValueEncrypted)
		if err != nil {
			return nil, err
		}
		out[v.Key] = effectiveEnvEntry{Value: plain, IsSecret: v.IsSecret}
	}
	return out, nil
}

// handlePutEnv replaces an app's entire env var set. An entry with
// is_secret=true and value=="" keeps whatever secret is already stored
// for that key (so the UI never has to round-trip a secret's plaintext
// just to leave it unchanged); every other entry's value is sealed fresh.
// The response is the GET shape, with header X-Basepod-Redeploy-Required
// set to "true" when the effective (decrypted) env set actually changed —
// env changes don't auto-redeploy a running container.
func (a *api) handlePutEnv(w http.ResponseWriter, r *http.Request) {
	app, ok := a.appBySlugOrNotFound(w, r)
	if !ok {
		return
	}

	var req []envVarResponse
	if !readJSON(w, r, &req) {
		return
	}

	for _, ev := range req {
		if !envKeyPattern.MatchString(ev.Key) {
			writeError(w, http.StatusUnprocessableEntity, "validation",
				"invalid env var key \""+ev.Key+"\": must match ^[A-Z_][A-Z0-9_]{0,63}$")
			return
		}
	}

	before, err := a.effectiveEnv(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load existing env vars")
		return
	}

	existing, err := a.st.ListEnvVars(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load existing env vars")
		return
	}
	existingByKey := make(map[string]store.EnvVar, len(existing))
	for _, ev := range existing {
		existingByKey[ev.Key] = ev
	}

	// Resolve every entry's sealed value up front, then persist the whole
	// desired set in one transactional call — ReplaceEnvVars upserts and
	// prunes atomically, so a mid-request failure can never leave a mix
	// of old and new env vars stored.
	desired := make([]store.EnvVar, 0, len(req))
	for _, ev := range req {
		var valueEncrypted string
		if ev.IsSecret && ev.Value == "" {
			if prior, ok := existingByKey[ev.Key]; ok && prior.IsSecret {
				// keep-on-empty-secret: preserve the previously stored
				// sealed value rather than overwriting it with an empty
				// secret.
				valueEncrypted = prior.ValueEncrypted
			} else {
				sealed, err := a.seal("")
				if err != nil {
					writeError(w, http.StatusInternalServerError, "internal", "failed to save env var")
					return
				}
				valueEncrypted = sealed
			}
		} else {
			sealed, err := a.seal(ev.Value)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to save env var")
				return
			}
			valueEncrypted = sealed
		}
		desired = append(desired, store.EnvVar{AppID: app.ID, Key: ev.Key, ValueEncrypted: valueEncrypted, IsSecret: ev.IsSecret})
	}

	if err := a.st.ReplaceEnvVars(app.ID, desired); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to save env vars")
		return
	}

	after, err := a.effectiveEnv(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load updated env vars")
		return
	}

	out, err := a.envResponseFor(app.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load updated env vars")
		return
	}

	changed := !envSetsEqual(before, after)
	w.Header().Set(redeployRequiredHeader, strconv.FormatBool(changed))
	writeJSON(w, http.StatusOK, out)
}

// envSetsEqual reports whether two decrypted env snapshots are identical
// in keys, values, and is_secret flags.
func envSetsEqual(a, b map[string]effectiveEnvEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av != bv {
			return false
		}
	}
	return true
}
