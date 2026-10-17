// Package cryptox provides the AES-256-GCM envelope used to protect
// credentials and other secrets at rest.
//
// Envelope format: base64( nonce || ciphertext ). Every Seal call draws a
// fresh random 96-bit nonce, as AES-GCM requires. The master key lives in a
// local key file (0600) or in the BARGAINPROXY_MASTER_KEY environment
// variable (base64, 32 bytes), environment taking priority.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	KeyLen    = 32
	EnvKeyVar = "BARGAINPROXY_MASTER_KEY"
)

// Box seals and opens small secrets with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// LoadOrCreateKey resolves the master key: environment variable first, then
// the key file, generating a new random key file on first use.
func LoadOrCreateKey(path string) ([]byte, error) {
	if env := os.Getenv(EnvKeyVar); env != "" {
		key, err := base64.StdEncoding.DecodeString(env)
		if err != nil || len(key) != KeyLen {
			return nil, fmt.Errorf("cryptox: %s must be base64 of %d bytes", EnvKeyVar, KeyLen)
		}
		return key, nil
	}
	if b, err := os.ReadFile(path); err == nil {
		if len(b) == KeyLen {
			return b, nil
		}
		if decoded, derr := base64.StdEncoding.DecodeString(string(b)); derr == nil && len(decoded) == KeyLen {
			return decoded, nil
		}
		return nil, fmt.Errorf("cryptox: key file %s has unexpected content", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("cryptox: want %d-byte key, got %d", KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns base64(nonce||ciphertext).
func (b *Box) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Open decrypts a value produced by Seal. An empty input yields empty output.
func (b *Box) Open(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("cryptox: ciphertext too short")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("cryptox: decrypt failed: %w", err)
	}
	return pt, nil
}
