package api

import (
	"encoding/base32"
	"fmt"
	"os"
	"strings"

	"dumpcat/internal/auth"
)

// SetupTokenFile is the file in the data directory that holds the one-time
// setup token while no account exists ("dumpcat setup-token" prints it).
const SetupTokenFile = "setup-token"

// setupTokenHint tells the user where to find the token.
const setupTokenHint = "it is printed in the DumpCat log when the service starts; on the server, run: dumpcat setup-token"

// PrepareSetup runs at startup. While no account exists it creates the
// one-time token that the first-run setup requires, so that whoever reaches
// the port first cannot claim the admin account. The token is written to path
// (0600) for "dumpcat setup-token" and returned so that it can be logged; it
// is still returned when the file cannot be written. Once an account exists
// it returns "" and removes the file.
func (s *Server) PrepareSetup(path string) (string, error) {
	setupMu.Lock()
	defer setupMu.Unlock()
	s.setupTokenPath = path
	n, err := s.Store.CountUsers()
	if err != nil {
		return "", err
	}
	if n > 0 {
		s.setupToken = ""
		removeSetupToken(path)
		return "", nil
	}
	tok := newSetupToken()
	s.setupToken = normalizeSetupToken(tok)
	if path == "" {
		return tok, nil
	}
	// recreate the file so that it never keeps looser permissions from before
	removeSetupToken(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = f.WriteString(tok + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return tok, fmt.Errorf("cannot write %s: %w", path, err)
	}
	return tok, nil
}

// newSetupToken returns 120 random bits as six groups of base32 letters and
// digits, e.g. "K7QM-2XTD-...", easy to copy or to type.
func newSetupToken() string {
	raw := base32.StdEncoding.EncodeToString(auth.RandomBytes(15))
	groups := make([]string, 0, len(raw)/4)
	for i := 0; i < len(raw); i += 4 {
		groups = append(groups, raw[i:i+4])
	}
	return strings.Join(groups, "-")
}

// normalizeSetupToken ignores case, spaces and dashes.
func normalizeSetupToken(t string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t', '\r', '\n':
			return -1
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, t)
}

func removeSetupToken(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// checkSetupToken verifies the token sent with the setup form. setupMu must be held.
func (s *Server) checkSetupToken(tok, ip string) error {
	if s.setupLimit == nil {
		s.setupLimit = auth.NewLimiter()
	}
	key := "ip:" + ip
	if d := s.setupLimit.Check(key); d > 0 {
		return errf(429, "too many incorrect setup tokens, try again in %d minutes", int(d.Minutes())+1)
	}
	if s.setupToken == "" {
		return errf(503, "no setup token is active — restart DumpCat to create one")
	}
	in := normalizeSetupToken(tok)
	if in == "" {
		return badRequest("the setup token is required: %s", setupTokenHint)
	}
	if !auth.ConstantTimeEqual(in, s.setupToken) {
		s.setupLimit.Fail(key)
		s.Store.Audit(nil, "", "setup_failed", "", "incorrect setup token", ip)
		return errf(403, "incorrect setup token: %s", setupTokenHint)
	}
	s.setupLimit.Reset(key)
	return nil
}

// endSetup invalidates the token once the first admin exists. setupMu must be held.
func (s *Server) endSetup() {
	s.setupToken = ""
	removeSetupToken(s.setupTokenPath)
}
