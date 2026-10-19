package store

import "testing"

func TestJobOptionsVerify(t *testing.T) {
	for _, c := range []struct {
		mode  string
		after bool
		want  string
	}{
		{"", false, VerifyOff}, // jobs saved before verify_mode
		{"", true, VerifyAll},
		{VerifyOne, false, VerifyOne},
		{VerifyOff, true, VerifyOff},
		{VerifyAll, false, VerifyAll},
		{"bogus", true, VerifyAll},
	} {
		if got := (JobOptions{VerifyMode: c.mode, VerifyAfter: c.after}).Verify(); got != c.want {
			t.Errorf("mode %q after %v: %q, want %q", c.mode, c.after, got, c.want)
		}
	}
}
