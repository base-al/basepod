package crypto

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := "hello, world!"
	sealed, err := Seal(key, plaintext)
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	opened, err := Open(key, sealed)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if opened != plaintext {
		t.Errorf("Roundtrip failed: expected %q, got %q", plaintext, opened)
	}
}

func TestSealDifferentNonces(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := "same plaintext"
	sealed1, err := Seal(key, plaintext)
	if err != nil {
		t.Fatalf("First Seal failed: %v", err)
	}

	sealed2, err := Seal(key, plaintext)
	if err != nil {
		t.Fatalf("Second Seal failed: %v", err)
	}

	if sealed1 == sealed2 {
		t.Error("Two seals of the same plaintext should differ due to random nonce")
	}
}

func TestOpenWithFlippedByte(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := "secret message"
	sealed, err := Seal(key, plaintext)
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	// Flip a byte in the sealed data
	sealedBytes := []byte(sealed)
	if len(sealedBytes) > 0 {
		if sealedBytes[0] == 'a' {
			sealedBytes[0] = 'b'
		} else {
			sealedBytes[0] = 'a'
		}
	}
	tamperedSealed := string(sealedBytes)

	_, err = Open(key, tamperedSealed)
	if err == nil {
		t.Error("Open with flipped byte should fail")
	}
}

func TestOpenWithDifferentKey(t *testing.T) {
	key1 := make([]byte, 32)
	for i := range key1 {
		key1[i] = byte(i)
	}

	key2 := make([]byte, 32)
	for i := range key2 {
		key2[i] = byte(i + 1)
	}

	plaintext := "secret message"
	sealed, err := Seal(key1, plaintext)
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	_, err = Open(key2, sealed)
	if err == nil {
		t.Error("Open with different key should fail")
	}
}

func TestOpenGarbageDoesNotPanic(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	garbage := "not-valid-base64-or-anything-useful-@#$%"
	_, err := Open(key, garbage)
	if err == nil {
		t.Error("Open with garbage should error")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()

	// First call should create the key
	key1, err := LoadOrCreateKey(tempDir)
	if err != nil {
		t.Fatalf("LoadOrCreateKey failed: %v", err)
	}

	if len(key1) != 32 {
		t.Errorf("Expected 32-byte key, got %d bytes", len(key1))
	}

	// Check that the file was created with 0600 permissions
	keyPath := filepath.Join(tempDir, "secret.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Key file not found: %v", err)
	}

	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("Key file should have 0600 permissions, got %o", mode)
	}

	// Second call should return the same key
	key2, err := LoadOrCreateKey(tempDir)
	if err != nil {
		t.Fatalf("Second LoadOrCreateKey failed: %v", err)
	}

	if !bytes.Equal(key1, key2) {
		t.Error("Second call to LoadOrCreateKey should return the same key")
	}
}

func TestLoadOrCreateKeyRejectsWrongSize(t *testing.T) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "secret.key")

	// Create a file with wrong size (31 bytes)
	wrongSizeKey := make([]byte, 31)
	for i := range wrongSizeKey {
		wrongSizeKey[i] = byte(i)
	}

	err := os.WriteFile(keyPath, wrongSizeKey, 0o600)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// LoadOrCreateKey should reject it
	_, err = LoadOrCreateKey(tempDir)
	if err == nil {
		t.Error("LoadOrCreateKey should reject existing file of wrong size")
	}
}

func TestLoadOrCreateKeyCreatesMissingParent(t *testing.T) {
	tempDir := t.TempDir()
	nestedDir := filepath.Join(tempDir, "nested", "dir", "structure")

	key, err := LoadOrCreateKey(nestedDir)
	if err != nil {
		t.Fatalf("LoadOrCreateKey failed to create parent directories: %v", err)
	}

	if len(key) != 32 {
		t.Errorf("Expected 32-byte key, got %d bytes", len(key))
	}

	keyPath := filepath.Join(nestedDir, "secret.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Key file not found in nested directory: %v", err)
	}

	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("Key file should have 0600 permissions, got %o", mode)
	}
}

// TestOpenWithTooShortDecodedData verifies that Open rejects valid base64 that
// decodes to fewer than 24 bytes (insufficient for nonce) without panicking.
func TestOpenWithTooShortDecodedData(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	// Create valid base64 that decodes to only 3 bytes (< 24 required for nonce)
	shortData := []byte("abc")
	shortBase64 := base64.RawStdEncoding.EncodeToString(shortData)

	_, err := Open(key, shortBase64)
	if err == nil {
		t.Error("Open with too-short decoded data should error, not panic")
	}
}

// TestSealWithWrongKeyLength verifies that Seal rejects keys that are not 32 bytes.
func TestSealWithWrongKeyLength(t *testing.T) {
	shortKey := make([]byte, 16) // Too short
	for i := range shortKey {
		shortKey[i] = byte(i)
	}

	_, err := Seal(shortKey, "plaintext")
	if err == nil {
		t.Error("Seal with wrong-length key should error")
	}
}

// TestOpenWithWrongKeyLength verifies that Open rejects keys that are not 32 bytes.
func TestOpenWithWrongKeyLength(t *testing.T) {
	rightKey := make([]byte, 32)
	for i := range rightKey {
		rightKey[i] = byte(i)
	}

	plaintext := "secret"
	sealed, err := Seal(rightKey, plaintext)
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}

	wrongKey := make([]byte, 24) // Wrong size
	for i := range wrongKey {
		wrongKey[i] = byte(i)
	}

	_, err = Open(wrongKey, sealed)
	if err == nil {
		t.Error("Open with wrong-length key should error")
	}
}
