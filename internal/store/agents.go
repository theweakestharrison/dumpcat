package store

import (
	"database/sql"
	"time"
)

const agentCols = `id,name,key_hash,key_prefix,created_at,updated_at,last_seen_at,last_ip,version,hostname,os`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var ca, ua int64
	var seen sql.NullInt64
	if err := row.Scan(&a.ID, &a.Name, &a.KeyHash, &a.KeyPrefix, &ca, &ua, &seen, &a.LastIP, &a.Version, &a.Hostname, &a.OS); err != nil {
		return nil, notFound(err)
	}
	a.CreatedAt, a.UpdatedAt = fromTS(ca), fromTS(ua)
	a.LastSeenAt = fromNullTS(seen)
	return &a, nil
}

func (s *Store) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) GetAgent(id int64) (*Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id=?`, id))
}

// AgentByKeyHash finds the agent a key (sha256 hex) belongs to.
func (s *Store) AgentByKeyHash(hash string) (*Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE key_hash=?`, hash))
}

func (s *Store) CreateAgent(a *Agent) error {
	t := now()
	res, err := s.db.Exec(`INSERT INTO agents(name,key_hash,key_prefix,created_at,updated_at) VALUES(?,?,?,?,?)`, a.Name, a.KeyHash, a.KeyPrefix, t, t)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	a.CreatedAt, a.UpdatedAt = fromTS(t), fromTS(t)
	return nil
}

func (s *Store) SetAgentKey(id int64, hash, prefix string) error {
	_, err := s.db.Exec(`UPDATE agents SET key_hash=?, key_prefix=?, updated_at=? WHERE id=?`, hash, prefix, now(), id)
	return err
}

func (s *Store) RenameAgent(id int64, name string) error {
	_, err := s.db.Exec(`UPDATE agents SET name=?, updated_at=? WHERE id=?`, name, now(), id)
	return err
}

// AgentSeen records a connection of the agent.
func (s *Store) AgentSeen(id int64, at time.Time, ip, version, hostname, os string) {
	_, _ = s.db.Exec(`UPDATE agents SET last_seen_at=?, last_ip=?, version=?, hostname=?, os=? WHERE id=?`, at.Unix(), ip, version, hostname, os, id)
}

func (s *Store) DeleteAgent(id int64) error {
	_, err := s.db.Exec(`DELETE FROM agents WHERE id=?`, id)
	return err
}

// AgentServers lists the names of the servers reached through an agent.
func (s *Store) AgentServers(id int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM servers WHERE agent_id=? ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
