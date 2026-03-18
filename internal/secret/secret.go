// Package secret provides AES-256-GCM encryption for sensitive config values.
// A random 32-byte key is generated once and persisted at a caller-supplied path
// (e.g. config/smtp.key) with mode 0600. Encrypted values are stored as
// "enc:<base64(nonce+ciphertext)>" so they are distinguishable from plaintext.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

const encPrefix = "enc:"

// LoadOrCreateKey returns the 32-byte AES key at keyPath, creating it if absent.
func LoadOrCreateKey(keyPath string) ([]byte, error) {
	data, err := os.ReadFile(keyPath)
	if err == nil && len(data) == 32 {
		return data, nil
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		return nil, fmt.Errorf("save secret key: %w", err)
	}
	return key, nil
}

// Encrypt encrypts plaintext with AES-256-GCM and returns "enc:<base64>".
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt decrypts a value produced by Encrypt and returns the plaintext.
func Decrypt(key []byte, encrypted string) (string, error) {
	if !IsEncrypted(encrypted) {
		return "", fmt.Errorf("value is not encrypted")
	}
	data, err := base64.StdEncoding.DecodeString(encrypted[len(encPrefix):])
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// IsEncrypted reports whether s was produced by Encrypt.
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, encPrefix)
}
