package auth

import (
	"testing"
	"time"
)

func TestTOTP(t *testing.T) {
	// RFC 6238 test vector (SHA1, secret "12345678901234567890")
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	code, err := TOTPCode(secret, time.Unix(59, 0))
	if err != nil || code != "287082" {
		t.Fatalf("got %s %v", code, err)
	}
	if !VerifyTOTP(secret, "287082", time.Unix(59+30, 0)) {
		t.Fatal("skew window")
	}
	if VerifyTOTP(secret, "287082", time.Unix(59+120, 0)) {
		t.Fatal("expired code accepted")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 4; i++ {
		l.Fail("k")
	}
	if l.Check("k") != 0 {
		t.Fatal("locked too early")
	}
	l.Fail("k")
	if l.Check("k") == 0 {
		t.Fatal("not locked")
	}
	l.Reset("k")
	if l.Check("k") != 0 {
		t.Fatal("reset")
	}
}

func TestPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	h, err := HashPassword("long enough pass")
	if err != nil || !CheckPassword(h, "long enough pass") || CheckPassword(h, "other") {
		t.Fatal("bcrypt")
	}
}

// Attempts are reserved before the password is checked: parallel requests get no extra tries,
// and a successful attempt gives its slot back.
func TestLimiterTake(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < l.MaxFails; i++ {
		if d := l.Take("k"); d > 0 {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if d := l.Take("k"); d == 0 {
		t.Fatal("one attempt too many was allowed")
	}
	if d := l.Take("k"); d == 0 {
		t.Fatal("the key must stay locked")
	}
	l2 := NewLimiter()
	for i := 0; i < 20; i++ {
		if d := l2.Take("ok"); d > 0 {
			t.Fatalf("successful attempts must not lock (attempt %d)", i+1)
		}
		l2.Release("ok")
	}
}

// A two-factor code signs in once.
func TestTOTPGuard(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Now()
	code, err := totpAt(secret, uint64(now.Unix()/30))
	if err != nil {
		t.Fatal(err)
	}
	var g TOTPGuard
	if !g.Use(1, secret, code, now) {
		t.Fatal("a valid code was refused")
	}
	if g.Use(1, secret, code, now) {
		t.Fatal("the same code worked twice")
	}
	if g.Use(2, secret, "000000", now) && code != "000000" {
		t.Fatal("a wrong code was accepted")
	}
}
