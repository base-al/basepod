package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const keySize = 32
const nonceSize = 24

// loadOrCreateKeyMu guards concurrent LoadOrCreateKey calls within the same process.
// Multiple goroutines attempting simultaneous key creation could generate different keys;
// this mutex ensures only one goroutine performs the actual write and file operations.
// Note: Multi-process first-run races remain best-effort (acceptable: single daemon owns dataDir).
var loadOrCreateKeyMu sync.Mutex

// LoadOrCreateKey loads or creates a 32-byte encryption key from <dataDir>/secret.key.
// It creates the parent directory structure if needed, writes the file with 0600 permissions,
// and returns the same key on subsequent calls.
// Within-process concurrency is serialized; callers always get the persisted key on disk.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	loadOrCreateKeyMu.Lock()
	defer loadOrCreateKeyMu.Unlock()

	keyPath := filepath.Join(dataDir, "secret.key")

	// Check if key file already exists (including after losing a race with another goroutine)
	if data, err := os.ReadFile(keyPath); err == nil {
		// File exists, validate its size
		if len(data) != keySize {
			return nil, fmt.Errorf("existing key file has wrong size: expected %d bytes, got %d bytes", keySize, len(data))
		}
		return data, nil
	} else if !os.IsNotExist(err) {
		// Some other error occurred
		return nil, fmt.Errorf("error reading key file: %w", err)
	}

	// Key file doesn't exist, create it
	// First, ensure the parent directory exists (0o700: owner read/write/execute only)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Generate a random 32-byte key
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	// Write the key to a temporary file first, then atomically rename to secret.key.
	// This prevents leaving a truncated key file on crash or disk-full errors.
	tempPath := keyPath + ".tmp"
	if err := os.WriteFile(tempPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write temporary key file: %w", err)
	}

	// Atomically rename the temporary file to the final location.
	if err := os.Rename(tempPath, keyPath); err != nil {
		// Clean up the temp file if rename failed
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("failed to finalize key file: %w", err)
	}

	// Re-read the key from disk and return it. This ensures that if another process
	// wrote a key between our generation and rename (multi-process race), we return
	// the one that's actually on disk, making the operation idempotent across processes.
	persistedKey, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read persisted key: %w", err)
	}

	if len(persistedKey) != keySize {
		return nil, fmt.Errorf("persisted key file has unexpected size: expected %d bytes, got %d bytes", keySize, len(persistedKey))
	}

	return persistedKey, nil
}

// AAD builds the additional-authenticated-data BasePod binds an env var
// ciphertext to (audit finding L9): an app's numeric ID and the env var's
// key name, joined by "|". Binding both into the AEAD tag means a
// ciphertext copy-pasted onto a different app's row, or onto a different
// key within the same row, fails to open under the new row/key's AAD
// (see SealAAD/OpenAAD) — closing the gap nil-AAD sealing left, where any
// sealed value could be relocated and still decrypt cleanly.
func AAD(appID int64, key string) []byte {
	return []byte(strconv.FormatInt(appID, 10) + "|" + key)
}

// Seal encrypts plaintext using XChaCha20-Poly1305 with a random 24-byte
// nonce and no additional authenticated data. It returns a
// base64.RawStdEncoding-encoded string of nonce||ciphertext.
//
// This is SealAAD with a nil aad — kept as its own entry point (rather
// than requiring every caller to pass nil explicitly) so existing callers
// keep compiling unchanged (see the package doc note below). New callers
// that have an app ID and env var key available should prefer SealAAD
// with AAD(appID, key) instead, so newly-written ciphertexts get the
// row-binding OpenAAD's migration path is built around.
func Seal(key []byte, plaintext string) (string, error) {
	return seal(key, plaintext, nil)
}

// SealAAD is Seal, additionally binding aad (see AAD) into the
// ciphertext's authentication tag: a ciphertext sealed under one aad
// value cannot be opened successfully under any other aad, including no
// aad at all (see OpenAAD's migration-path fallback, which is the one
// deliberate exception: it also accepts nil-aad legacy ciphertexts).
func SealAAD(key []byte, plaintext string, aad []byte) (string, error) {
	return seal(key, plaintext, aad)
}

func seal(key []byte, plaintext string, aad []byte) (string, error) {
	if len(key) != keySize {
		return "", fmt.Errorf("key must be %d bytes, got %d", keySize, len(key))
	}

	// Create cipher
	cipher, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Generate random nonce
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Encrypt plaintext
	ciphertext := cipher.Seal(nil, nonce, []byte(plaintext), aad)

	// Combine nonce and ciphertext
	combined := append(nonce, ciphertext...)

	// Encode to base64.RawStdEncoding
	encoded := base64.RawStdEncoding.EncodeToString(combined)

	return encoded, nil
}

// Open decrypts a sealed message that was created by Seal (no AAD).
//
// This is a thin wrapper over open(key, sealed, nil) — kept as its own
// entry point, with its signature unchanged, specifically so every
// existing caller outside this package (internal/server's seal/open
// closures, internal/api's tests) keeps compiling as-is; see OpenAAD for
// the AAD-verifying entry point new callers should move to, and the
// FOLLOW-UP note on OpenAAD for what that migration actually requires.
func Open(key []byte, sealed string) (string, error) {
	return open(key, sealed, nil)
}

// OpenAAD is Open, additionally verifying aad (see AAD) against the
// ciphertext's authentication tag — with a migration-path fallback: it
// tries aad first, and only if that fails (wrong tag) does it retry with
// no AAD at all, matching how every row sealed before AAD binding existed
// (via the plain Seal/Open above) was written. This lazy-upgrade shape
// means existing ciphertexts keep decrypting exactly as before, while
// anything newly sealed through SealAAD is bound to its aad — a copy of
// it relocated to a different row fails both the aad-bound attempt (wrong
// aad) AND the legacy fallback (it was never sealed with nil aad), so
// unlike Open's original callers is safe against exactly the accidental
// relocation the L9 audit finding filed.
//
// FOLLOW-UP (out of scope for this file's owner, tracked for whoever owns
// internal/api/env.go and internal/store next): nothing outside
// internal/crypto calls SealAAD/OpenAAD yet. Actually closing the gap
// requires internal/api's seal/open closures (wired in internal/server)
// to start calling SealAAD(key, plaintext, crypto.AAD(appID, key)) on
// write and OpenAAD(key, sealed, crypto.AAD(appID, key)) on read — which
// needs the app ID and env var key threaded through to those closures,
// currently just func(string) (string, error). Until that lands, this
// function is exercised only by this package's own tests.
func OpenAAD(key []byte, sealed string, aad []byte) (string, error) {
	plaintext, err := open(key, sealed, aad)
	if err == nil {
		return plaintext, nil
	}
	if len(aad) == 0 {
		return "", err
	}
	// Legacy fallback: retry with no AAD, matching every row sealed before
	// AAD binding existed.
	if legacyPlaintext, legacyErr := open(key, sealed, nil); legacyErr == nil {
		return legacyPlaintext, nil
	}
	return "", err
}

func open(key []byte, sealed string, aad []byte) (string, error) {
	if len(key) != keySize {
		return "", fmt.Errorf("key must be %d bytes, got %d", keySize, len(key))
	}

	// Decode from base64.RawStdEncoding
	combined, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("failed to decode sealed message: %w", err)
	}

	// Extract nonce and ciphertext
	if len(combined) < nonceSize {
		return "", fmt.Errorf("sealed message too short: expected at least %d bytes, got %d", nonceSize, len(combined))
	}

	nonce := combined[:nonceSize]
	ciphertext := combined[nonceSize:]

	// Create cipher
	cipher, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Decrypt
	plaintext, err := cipher.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt message: %w", err)
	}

	return string(plaintext), nil
}
