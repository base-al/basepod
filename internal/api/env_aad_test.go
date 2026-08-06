package api

import (
	"net/http"
	"testing"

	"github.com/base-al/basepod/internal/crypto"
)

// TestEnvPutSealsWithAAD proves a fresh PUT seals every value bound to
// this app's ID and the var's key (audit finding L9): the resulting
// ciphertext must reject a plain no-AAD open, and must open correctly
// under the exact crypto.AAD(appID, key) binding.
func TestEnvPutSealsWithAAD(t *testing.T) {
	srv, st, token, appID, _ := setupEnvDomainsTest(t)

	put := []envVarResponse{{Key: "PORT", Value: "8080", IsSecret: false}}
	var putResp []envVarResponse
	resp := doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put, &putResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put: got status %d, want 200", resp.StatusCode)
	}

	stored, err := st.ListEnvVars(appID)
	if err != nil {
		t.Fatal(err)
	}
	sealed := envVarByKey(stored, "PORT").ValueEncrypted

	if _, err := crypto.Open(testKey, sealed); err == nil {
		t.Fatal("expected a fresh write's ciphertext to reject a plain no-AAD open")
	}
	plain, err := crypto.OpenAAD(testKey, sealed, crypto.AAD(appID, "PORT"))
	if err != nil {
		t.Fatalf("OpenAAD with the correct (appID, key) binding: %v", err)
	}
	if plain != "8080" {
		t.Fatalf("plain = %q, want 8080", plain)
	}
}

// TestEnvGetDecryptsLegacyNoAADRow proves a row sealed before AAD binding
// existed (plain crypto.Seal, no AAD — what every row looked like prior
// to this change) still decrypts correctly through the API's GET path,
// via OpenAAD's legacy-fallback.
func TestEnvGetDecryptsLegacyNoAADRow(t *testing.T) {
	srv, st, token, appID, _ := setupEnvDomainsTest(t)

	legacy, err := crypto.Seal(testKey, "legacy-value")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEnvVar(appID, "LEGACY", legacy, false); err != nil {
		t.Fatal(err)
	}

	var got []envVarResponse
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/my-blog/env", token, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: got status %d, want 200", resp.StatusCode)
	}
	if envByKey(got)["LEGACY"].Value != "legacy-value" {
		t.Fatalf("expected the legacy (no-AAD) row to still decrypt, got %+v", got)
	}
}

// TestEnvRelocatedRowFailsToDecrypt proves a sealed value copied from one
// app's row onto another app's row (e.g. by a DB-write attacker, or a
// backup/restore mistake) fails to decrypt: it stays bound to app A's ID
// as AEAD additional-authenticated-data, so relocating it under app B's
// row changes the AAD OpenAAD verifies against and the AEAD tag no
// longer matches — closing the gap nil-AAD sealing left (audit finding
// L9).
func TestEnvRelocatedRowFailsToDecrypt(t *testing.T) {
	srv, st, token, appAID, _ := setupEnvDomainsTest(t)

	put := []envVarResponse{{Key: "SHARED", Value: "app-a-value", IsSecret: false}}
	var putResp []envVarResponse
	if resp := doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put, &putResp); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: got status %d, want 200", resp.StatusCode)
	}
	storedA, err := st.ListEnvVars(appAID)
	if err != nil {
		t.Fatal(err)
	}
	sealedOnA := envVarByKey(storedA, "SHARED").ValueEncrypted

	// Create a second app directly through the store (bypassing the API
	// so this test doesn't need a second login) and simulate a DB-write
	// attacker relocating app A's sealed ciphertext onto app B's row
	// under the same key.
	appB, err := st.CreateApp("app-b", "nginx:alpine", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEnvVar(appB.ID, "SHARED", sealedOnA, false); err != nil {
		t.Fatal(err)
	}

	// Direct proof at the crypto layer: opening app A's ciphertext under
	// app B's AAD must fail outright (no legacy fallback rescues it,
	// since it was never sealed with nil AAD).
	if _, err := crypto.OpenAAD(testKey, sealedOnA, crypto.AAD(appB.ID, "SHARED")); err == nil {
		t.Fatal("expected OpenAAD under app B's binding to fail for a ciphertext sealed under app A's binding")
	}

	// End-to-end proof through the API: GET on app B, which now holds
	// app A's relocated ciphertext under the same key, must fail to
	// decrypt it (surfaced as a 500 from envResponseFor's open call).
	resp := doJSON(t, http.MethodGet, srv.URL+"/api/v1/apps/app-b/env", token, nil, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("get on the relocated row: got status %d, want 500 (decrypt should fail)", resp.StatusCode)
	}
}

// TestEnvPutUpgradesLegacyRowToAAD proves the lazy-upgrade path: a
// pre-existing legacy (no-AAD) row that a PUT actually writes a new
// value for comes out the other side AAD-bound, not legacy — every write
// goes through the AAD-aware seal closure, so there is no separate
// migration step.
func TestEnvPutUpgradesLegacyRowToAAD(t *testing.T) {
	srv, st, token, appID, _ := setupEnvDomainsTest(t)

	legacy, err := crypto.Seal(testKey, "old-value")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEnvVar(appID, "UPGRADE_ME", legacy, false); err != nil {
		t.Fatal(err)
	}

	put := []envVarResponse{{Key: "UPGRADE_ME", Value: "new-value", IsSecret: false}}
	var putResp []envVarResponse
	if resp := doJSON(t, http.MethodPut, srv.URL+"/api/v1/apps/my-blog/env", token, put, &putResp); resp.StatusCode != http.StatusOK {
		t.Fatalf("put: got status %d, want 200", resp.StatusCode)
	}

	stored, err := st.ListEnvVars(appID)
	if err != nil {
		t.Fatal(err)
	}
	sealed := envVarByKey(stored, "UPGRADE_ME").ValueEncrypted
	if _, err := crypto.Open(testKey, sealed); err == nil {
		t.Fatal("expected the re-sealed row to reject a plain no-AAD open (it should now be AAD-bound, not legacy)")
	}
	plain, err := crypto.OpenAAD(testKey, sealed, crypto.AAD(appID, "UPGRADE_ME"))
	if err != nil {
		t.Fatalf("OpenAAD after upgrade: %v", err)
	}
	if plain != "new-value" {
		t.Fatalf("plain = %q, want new-value", plain)
	}
}
