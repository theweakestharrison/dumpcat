package store

import "strings"

// SSOConfig is the OpenID Connect sign-in (Settings → Authentication).
type SSOConfig struct {
	Enabled        bool   `json:"enabled"`
	Issuer         string `json:"issuer"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`   // encrypted at rest
	Scopes         string `json:"scopes"`          // space-separated, with openid
	ButtonLabel    string `json:"button_label"`    // "" = Sign in with SSO
	EmailClaim     string `json:"email_claim"`     // "" = email
	AllowedDomains string `json:"allowed_domains"` // comma-separated; "" = any
	AutoCreate     bool   `json:"auto_create"`     // create unknown users with DefaultRole
	DefaultRole    string `json:"default_role"`
	// role mapping: the claim (groups, roles, realm_access.roles...) and the values that give each
	// role (comma-separated); "" = no mapping, roles are managed in DumpCat
	RoleClaim      string `json:"role_claim"`
	AdminValues    string `json:"admin_values"`
	OperatorValues string `json:"operator_values"`
	ViewerValues   string `json:"viewer_values"`
	// with SSO on, only admins may still sign in with a password (the fallback when the provider is down)
	PasswordAdminsOnly bool `json:"password_admins_only"`
}

const ssoKV = "sso"

// GetSSO returns the SSO settings with the client secret decrypted.
func (s *Store) GetSSO() SSOConfig {
	c := SSOConfig{DefaultRole: RoleViewer}
	if v, ok := s.GetKV(ssoKV); ok {
		fromJSON(v, &c)
		c.ClientSecret = s.dec(c.ClientSecret)
	}
	return c
}

func (s *Store) SaveSSO(c SSOConfig) error {
	sec, err := s.enc(c.ClientSecret)
	if err != nil {
		return err
	}
	c.ClientSecret = sec
	return s.SetKV(ssoKV, toJSON(c))
}

// GetUserBySSOSubject finds the user linked to an identity provider's account (issuer + subject).
func (s *Store) GetUserBySSOSubject(subject string) (*User, error) {
	if subject == "" {
		return nil, ErrNotFound
	}
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE sso_subject=?`, subject))
}

// FindUsersByEmail lists the users with an email address (case does not matter).
func (s *Store) FindUsersByEmail(email string) ([]User, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT `+userCols+` FROM users WHERE lower(trim(email))=lower(?) ORDER BY id`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := s.scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}
