// Package secret encrypts sensitive values (DB passwords, cloud credentials,
// encryption passphrases) at rest using AES-256-GCM with a master key.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "enc:v1:"

type Box struct {
	aead cipher.AEAD
}

// Load reads the master key from path (creating it if missing) unless override
// (base64, 32 bytes) is provided.
func Load(path, override string) (*Box, error) {
	var key []byte
	switch {
	case override != "":
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(override))
		if err != nil {
			return nil, fmt.Errorf("master key: %w", err)
		}
		key = k
	default:
		data, err := os.ReadFile(path)
		if err == nil {
			k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
			if err != nil {
				return nil, fmt.Errorf("master key file %s: %w", path, err)
			}
			key = k
		} else if errors.Is(err, os.ErrNotExist) {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
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

func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

// MustEncrypt is a convenience for values that must not fail silently.
func (b *Box) MustEncrypt(plain string) string {
	s, err := b.Encrypt(plain)
	if err != nil {
		panic(err)
	}
	return s
}

func (b *Box) Decrypt(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, prefix) {
		return s, nil // stored in clear (legacy / migration)
	}
	raw, err := base64.RawStdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt secret (wrong master key?)")
	}
	return string(pt), nil
}
