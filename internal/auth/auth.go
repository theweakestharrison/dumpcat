// Package auth contains password hashing, random tokens, TOTP (RFC 6238)
// and a login rate limiter.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

func HashPassword(p string) (string, error) {
	if err := ValidatePassword(p); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcryptCost)
	return string(h), err
}

func CheckPassword(hash, p string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(p)) == nil
}

// dummyHash is used to keep timing similar when the user does not exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcryptCost)

func BurnTime(p string) { _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(p)) }

func ValidatePassword(p string) error {
	n := utf8.RuneCountInString(p)
	if n < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if len(p) > 72 {
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func RandomToken(n int) string {
	return base64.RawURLEncoding.EncodeToString(RandomBytes(n))
}

func SHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- TOTP ----

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string { return b32.EncodeToString(RandomBytes(20)) }

func totpAt(secret string, counter uint64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code), nil
}

func TOTPCode(secret string, t time.Time) (string, error) {
	return totpAt(secret, uint64(t.Unix()/30))
}

// VerifyTOTP accepts the current code and one step before/after (clock skew).
func VerifyTOTP(secret, code string, t time.Time) bool {
	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(code) != 6 || secret == "" {
		return false
	}
	c := t.Unix() / 30
	for _, d := range []int64{0, -1, 1} {
		want, err := totpAt(secret, uint64(c+d))
		if err == nil && ConstantTimeEqual(want, code) {
			return true
		}
	}
	return false
}

func TOTPURL(issuer, account, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// ---- rate limiter ----

type attempt struct {
	fails       int
	first       time.Time
	lockedUntil time.Time
}

type Limiter struct {
	mu       sync.Mutex
	m        map[string]*attempt
	MaxFails int
	Window   time.Duration
	Lockout  time.Duration
}

func NewLimiter() *Limiter {
	return &Limiter{m: map[string]*attempt{}, MaxFails: 5, Window: 15 * time.Minute, Lockout: 15 * time.Minute}
}

// Check returns the remaining lock duration (0 = allowed).
func (l *Limiter) Check(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[key]
	if a == nil {
		return 0
	}
	if d := time.Until(a.lockedUntil); d > 0 {
		return d
	}
	return 0
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a := l.m[key]
	if a == nil || now.Sub(a.first) > l.Window {
		a = &attempt{first: now}
		l.m[key] = a
	}
	a.fails++
	if a.fails >= l.MaxFails {
		a.lockedUntil = now.Add(l.Lockout)
		a.fails = 0
		a.first = now
	}
	// opportunistic cleanup
	if len(l.m) > 10000 {
		for k, v := range l.m {
			if now.Sub(v.first) > l.Window && now.After(v.lockedUntil) {
				delete(l.m, k)
			}
		}
	}
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}
