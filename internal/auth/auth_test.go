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
