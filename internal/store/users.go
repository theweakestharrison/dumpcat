package store

import (
	"database/sql"
	"time"
)

const userCols = `id,username,email,password_hash,role,totp_secret,totp_enabled,disabled,created_at,updated_at,last_login_at,sso_subject`

func (s *Store) scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var totp string
	var te, dis int
	var ca, ua int64
	var ll sql.NullInt64
	if err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &totp, &te, &dis, &ca, &ua, &ll, &u.SSOSubject); err != nil {
		return nil, notFound(err)
	}
	u.TOTPSecret = s.dec(totp)
	u.TOTPEnabled = te == 1
	u.Disabled = dis == 1
	u.CreatedAt = fromTS(ca)
	u.UpdatedAt = fromTS(ua)
	u.LastLoginAt = fromNullTS(ll)
	return &u, nil
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND disabled=0`).Scan(&n)
	return n, err
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
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

func (s *Store) GetUser(id int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) GetUserByName(name string) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, name))
}

func (s *Store) CreateUser(u *User) error {
	t := now()
	totp, err := s.enc(u.TOTPSecret)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`INSERT INTO users(username,email,password_hash,role,totp_secret,totp_enabled,disabled,created_at,updated_at,sso_subject) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		u.Username, u.Email, u.PasswordHash, u.Role, totp, b2i(u.TOTPEnabled), b2i(u.Disabled), t, t, u.SSOSubject)
	if err != nil {
		return err
	}
	u.ID, _ = res.LastInsertId()
	u.CreatedAt, u.UpdatedAt = fromTS(t), fromTS(t)
	return nil
}

// UpdateUser saves all mutable fields.
func (s *Store) UpdateUser(u *User) error {
	totp, err := s.enc(u.TOTPSecret)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET username=?,email=?,password_hash=?,role=?,totp_secret=?,totp_enabled=?,disabled=?,updated_at=?,sso_subject=? WHERE id=?`,
		u.Username, u.Email, u.PasswordHash, u.Role, totp, b2i(u.TOTPEnabled), b2i(u.Disabled), now(), u.SSOSubject, u.ID)
	return err
}

func (s *Store) TouchLogin(id int64) {
	_, _ = s.db.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, now(), id)
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

// ---- sessions ----

func (s *Store) CreateSession(se *Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions(id,user_id,csrf,ip,user_agent,created_at,last_seen_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		se.ID, se.UserID, se.CSRF, se.IP, se.UserAgent, se.CreatedAt.Unix(), se.LastSeenAt.Unix(), se.ExpiresAt.Unix())
	return err
}

func (s *Store) GetSession(id string) (*Session, error) {
	var se Session
	var ca, ls, ex int64
	err := s.db.QueryRow(`SELECT id,user_id,csrf,ip,user_agent,created_at,last_seen_at,expires_at FROM sessions WHERE id=?`, id).
		Scan(&se.ID, &se.UserID, &se.CSRF, &se.IP, &se.UserAgent, &ca, &ls, &ex)
	if err != nil {
		return nil, notFound(err)
	}
	se.CreatedAt, se.LastSeenAt, se.ExpiresAt = fromTS(ca), fromTS(ls), fromTS(ex)
	return &se, nil
}

func (s *Store) TouchSession(id string, lastSeen, expires time.Time) {
	_, _ = s.db.Exec(`UPDATE sessions SET last_seen_at=?, expires_at=? WHERE id=?`, lastSeen.Unix(), expires.Unix(), id)
}

func (s *Store) DeleteSession(id string) {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
}

func (s *Store) DeleteUserSessions(userID int64, exceptID string) {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE user_id=? AND id<>?`, userID, exceptID)
}

// DeleteUserTokens revokes every API token of a user (password reset by an admin, account
// disabled): a token made with a stolen session must not outlive the recovery.
func (s *Store) DeleteUserTokens(userID int64) {
	_, _ = s.db.Exec(`DELETE FROM api_tokens WHERE user_id=?`, userID)
}

func (s *Store) PurgeSessions() {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now())
}

// ---- API tokens ----

func (s *Store) CreateToken(t *APIToken) error {
	ts := now()
	if t.Access != TokenFull && t.Access != TokenManage {
		t.Access = TokenRead
	}
	res, err := s.db.Exec(`INSERT INTO api_tokens(user_id,name,prefix,token_hash,access,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
		t.UserID, t.Name, t.Prefix, t.Hash, t.Access, ts, nullTS(t.ExpiresAt))
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	t.CreatedAt = fromTS(ts)
	return nil
}

func (s *Store) ListTokens(userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT id,user_id,name,prefix,token_hash,access,created_at,last_used_at,expires_at FROM api_tokens WHERE user_id=? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var ca int64
		var lu, ex sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Hash, &t.Access, &ca, &lu, &ex); err != nil {
			return nil, err
		}
		t.CreatedAt = fromTS(ca)
		t.LastUsedAt = fromNullTS(lu)
		t.ExpiresAt = fromNullTS(ex)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTokenByHash(hash string) (*APIToken, error) {
	var t APIToken
	var ca int64
	var lu, ex sql.NullInt64
	err := s.db.QueryRow(`SELECT id,user_id,name,prefix,token_hash,access,created_at,last_used_at,expires_at FROM api_tokens WHERE token_hash=?`, hash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Hash, &t.Access, &ca, &lu, &ex)
	if err != nil {
		return nil, notFound(err)
	}
	t.CreatedAt = fromTS(ca)
	t.LastUsedAt = fromNullTS(lu)
	t.ExpiresAt = fromNullTS(ex)
	return &t, nil
}

func (s *Store) TouchToken(id int64) {
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at=? WHERE id=?`, now(), id)
}

func (s *Store) DeleteToken(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id=? AND user_id=?`, id, userID)
	return err
}
