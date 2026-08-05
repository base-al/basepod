package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

const keySize = 32
const nonceSize = 24

// LoadOrCreateKey loads or creates a 32-byte encryption key from <dataDir>/secret.key.
// It creates the parent directory structure if needed, writes the file with 0600 permissions,
// and returns the same key on subsequent calls.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	keyPath := filepath.Join(dataDir, "secret.key")

	// Check if key file already exists
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
	// First, ensure the parent directory exists
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	// Generate a random 32-byte key
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	// Write the key to file with 0600 permissions
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("failed to write key file: %w", err)
	}

	return key, nil
}

// Seal encrypts plaintext using XChaCha20-Poly1305 with a random 24-byte nonce.
// It returns a base64.RawStdEncoding-encoded string of nonce||ciphertext.
func Seal(key []byte, plaintext string) (string, error) {
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
	ciphertext := cipher.Seal(nil, nonce, []byte(plaintext), nil)

	// Combine nonce and ciphertext
	combined := append(nonce, ciphertext...)

	// Encode to base64.RawStdEncoding
	encoded := base64.RawStdEncoding.EncodeToString(combined)

	return encoded, nil
}

// Open decrypts a sealed message that was created by Seal.
// It expects the sealed message to be a base64.RawStdEncoding-encoded string of nonce||ciphertext.
func Open(key []byte, sealed string) (string, error) {
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
	plaintext, err := cipher.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt message: %w", err)
	}

	return string(plaintext), nil
}
