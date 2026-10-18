package update

// Release signing. The publisher signs the exact bytes of latest.json with an
// ed25519 private key and uploads the signature next to it as latest.json.sig
// (base64, one signature per line). Installations that trust at least one public
// key refuse every release whose latest.json no trusted key has signed; since
// latest.json carries the SHA-256 of every binary, the binaries are covered too.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SignatureSuffix is appended to the manifest URL / file name for its signature.
const SignatureSuffix = ".sig"

const (
	maxSignature   = 4 << 10 // latest.json.sig
	maxSignatures  = 8       // lines in latest.json.sig
	privateKeySize = ed25519.PrivateKeySize
)

// SigningInfo tells the UI whether releases must be signed.
type SigningInfo struct {
	Required bool     `json:"required"`       // a release must be signed by one of Keys
	Keys     []string `json:"keys,omitempty"` // trusted public keys (base64)
}

// Signing describes the verification done with keys.
func Signing(keys []ed25519.PublicKey) SigningInfo {
	info := SigningInfo{Required: len(keys) > 0}
	for _, k := range keys {
		info.Keys = append(info.Keys, EncodePublicKey(k))
	}
	return info
}

// EncodePublicKey returns the base64 form used in ldflags, env and the UI.
func EncodePublicKey(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// ParsePublicKey decodes one base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("public key %q is not valid base64", short(s))
	}
	switch len(b) {
	case ed25519.PublicKeySize:
		return ed25519.PublicKey(b), nil
	case privateKeySize:
		// never echo any part of it
		return nil, errors.New("a PRIVATE signing key was given where a public key is expected — keep it secret and use the .pub file")
	}
	return nil, fmt.Errorf("public key %q has %d bytes, an ed25519 public key has %d", short(s), len(b), ed25519.PublicKeySize)
}

// ParsePublicKeys decodes a list of base64 public keys separated by commas,
// semicolons or white space. Duplicates are dropped; an empty list is not an error.
func ParsePublicKeys(list ...string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, s := range list {
		for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
			k, err := ParsePublicKey(f)
			if err != nil {
				return nil, err
			}
			dup := false
			for _, have := range keys {
				if have.Equal(k) {
					dup = true
					break
				}
			}
			if !dup {
				keys = append(keys, k)
			}
		}
	}
	return keys, nil
}

// GenerateKey creates a new signing key pair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePrivateKey returns the content of a private key file (base64 of the
// 64-byte key, so that it cannot be mistaken for a public key).
func EncodePrivateKey(k ed25519.PrivateKey) []byte {
	return []byte(base64.StdEncoding.EncodeToString(k) + "\n")
}

// ParsePrivateKey decodes a private key file. A bare 32-byte value is refused:
// it is most likely the public key, which would make a key anyone can derive.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, errors.New("the signing key file is not valid base64")
	}
	switch len(b) {
	case privateKeySize:
		k := ed25519.PrivateKey(b)
		if !bytes.Equal(ed25519.NewKeyFromSeed(k.Seed()), k) {
			return nil, errors.New("the signing key file is corrupt")
		}
		return k, nil
	case ed25519.PublicKeySize:
		return nil, errors.New("this is a public key (.pub), not the private signing key")
	}
	return nil, fmt.Errorf("the signing key file holds %d bytes, not an ed25519 private key", len(b))
}

// Sign returns one line of latest.json.sig: the base64 signature of manifest.
func Sign(k ed25519.PrivateKey, manifest []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(k, manifest)) + "\n")
}

// Verify checks that one of the signatures in sig (a latest.json.sig file) was
// made by one of keys over manifest, and returns that key.
func Verify(manifest, sig []byte, keys []ed25519.PublicKey) (ed25519.PublicKey, error) {
	if len(keys) == 0 {
		return nil, errors.New("no public key to verify the signature with")
	}
	lines := strings.Fields(string(sig))
	if len(lines) == 0 {
		return nil, errors.New("the signature file is empty")
	}
	if len(lines) > maxSignatures {
		return nil, fmt.Errorf("the signature file has %d lines (at most %d)", len(lines), maxSignatures)
	}
	valid := false
	for _, l := range lines {
		s, err := base64.StdEncoding.DecodeString(l)
		if err != nil || len(s) != ed25519.SignatureSize {
			continue
		}
		valid = true
		for _, k := range keys {
			if ed25519.Verify(k, manifest, s) {
				return k, nil
			}
		}
	}
	if !valid {
		return nil, errors.New("the signature file does not contain an ed25519 signature")
	}
	return nil, errors.New("latest.json is not signed by a trusted key (tampered with, or signed with another key)")
}

func short(s string) string {
	if len(s) > 12 {
		return s[:8] + "…"
	}
	return s
}
