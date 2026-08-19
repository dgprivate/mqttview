// Package secrets encrypts values that must be stored at rest but used in
// plaintext later — chiefly the passwords mqttview presents to MQTT brokers.
// Hashing is not an option there, so we use authenticated encryption with a
// server-held key.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoKey is returned when a Box is used without a configured key.
var ErrNoKey = errors.New("secrets: no encryption key configured")

// Box seals and opens secrets with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a 32-byte key encoded as hex or standard base64.
func New(encodedKey string) (*Box, error) {
	key, err := decodeKey(encodedKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// LoadOrCreateKey returns the key from encodedKey, or reads/generates a
// persistent key file in dataDir when encodedKey is empty. The generated file
// is written 0600 so it is not world-readable.
func LoadOrCreateKey(encodedKey, dataDir string) (string, error) {
	if encodedKey != "" {
		if _, err := decodeKey(encodedKey); err != nil {
			return "", err
		}
		return encodedKey, nil
	}

	path := filepath.Join(dataDir, "secret.key")
	raw, err := os.ReadFile(path)
	if err == nil {
		trimmed := strings.TrimSpace(string(raw))
		if _, err := decodeKey(trimmed); err != nil {
			return "", fmt.Errorf("secrets: %s is unusable: %w", path, err)
		}
		return trimmed, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("secrets: read %s: %w", path, err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("secrets: generate key: %w", err)
	}
	encoded := hex.EncodeToString(key)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("secrets: create %s: %w", dataDir, err)
	}
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("secrets: write %s: %w", path, err)
	}
	return encoded, nil
}

// Seal encrypts plaintext and returns a base64 string safe for the database.
// Empty input stays empty so callers can round-trip "no password" cleanly.
func (b *Box) Seal(plaintext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrNoKey
	}
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open reverses Seal.
func (b *Box) Open(ciphertext string) (string, error) {
	if b == nil || b.aead == nil {
		return "", ErrNoKey
	}
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("secrets: decode: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("secrets: ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// Usually means the key changed since the value was written.
		return "", fmt.Errorf("secrets: open: %w", err)
	}
	return string(plain), nil
}

func decodeKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, ErrNoKey
	}
	if key, err := hex.DecodeString(encoded); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, errors.New("secrets: key must be 32 bytes encoded as hex or base64")
}
