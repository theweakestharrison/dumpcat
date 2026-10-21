package store

import (
	"database/sql"
)

const serverCols = `id,name,host,port,socket,username,password,tls_mode,ca_cert,flavor,version,datadir,service_name,os_user,notes,created_at,updated_at,restore_username,restore_password,agent_id,wg_peer_id,engine,backup_dir,backup_dir_local`

func (s *Store) scanServer(row interface{ Scan(...any) error }) (*Server, error) {
	var sv Server
	var pw, rpw string
	var ca, ua int64
	var aid, wid sql.NullInt64
	if err := row.Scan(&sv.ID, &sv.Name, &sv.Host, &sv.Port, &sv.Socket, &sv.Username, &pw, &sv.TLSMode, &sv.CACert,
		&sv.Flavor, &sv.Version, &sv.Datadir, &sv.ServiceName, &sv.OSUser, &sv.Notes, &ca, &ua, &sv.RestoreUsername, &rpw, &aid, &wid, &sv.Engine, &sv.BackupDir, &sv.BackupDirLocal); err != nil {
		return nil, notFound(err)
	}
	sv.AgentID = fromNullID(aid)
	sv.WGPeerID = fromNullID(wid)
	sv.Password = s.dec(pw)
	sv.HasPassword = sv.Password != ""
	sv.RestorePassword = s.dec(rpw)
	sv.HasRestorePassword = sv.RestorePassword != ""
	sv.CreatedAt, sv.UpdatedAt = fromTS(ca), fromTS(ua)
	return &sv, nil
}

func (s *Store) ListServers() ([]Server, error) {
	rows, err := s.db.Query(`SELECT ` + serverCols + ` FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		sv, err := s.scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sv)
	}
	return out, rows.Err()
}

func (s *Store) GetServer(id int64) (*Server, error) {
	return s.scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE id=?`, id))
}

func (s *Store) CreateServer(sv *Server) error {
	pw, err := s.enc(sv.Password)
	if err != nil {
		return err
	}
	rpw, err := s.enc(sv.RestorePassword)
	if err != nil {
		return err
	}
	t := now()
	res, err := s.db.Exec(`INSERT INTO servers(name,host,port,socket,username,password,tls_mode,ca_cert,flavor,version,datadir,service_name,os_user,notes,created_at,updated_at,restore_username,restore_password,agent_id,wg_peer_id,engine,backup_dir,backup_dir_local)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sv.Name, sv.Host, sv.Port, sv.Socket, sv.Username, pw, sv.TLSMode, sv.CACert, sv.Flavor, sv.Version, sv.Datadir, sv.ServiceName, sv.OSUser, sv.Notes, t, t, sv.RestoreUsername, rpw, nullID(sv.AgentID), nullID(sv.WGPeerID), sv.EngineName(), sv.BackupDir, sv.BackupDirLocal)
	if err != nil {
		return err
	}
	sv.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateServer(sv *Server) error {
	pw, err := s.enc(sv.Password)
	if err != nil {
		return err
	}
	rpw, err := s.enc(sv.RestorePassword)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE servers SET name=?,host=?,port=?,socket=?,username=?,password=?,tls_mode=?,ca_cert=?,flavor=?,version=?,datadir=?,service_name=?,os_user=?,notes=?,updated_at=?,restore_username=?,restore_password=?,agent_id=?,wg_peer_id=?,engine=?,backup_dir=?,backup_dir_local=? WHERE id=?`,
		sv.Name, sv.Host, sv.Port, sv.Socket, sv.Username, pw, sv.TLSMode, sv.CACert, sv.Flavor, sv.Version, sv.Datadir, sv.ServiceName, sv.OSUser, sv.Notes, now(), sv.RestoreUsername, rpw, nullID(sv.AgentID), nullID(sv.WGPeerID), sv.EngineName(), sv.BackupDir, sv.BackupDirLocal, sv.ID)
	return err
}

func (s *Store) UpdateServerDetected(id int64, version, datadir string) {
	_, _ = s.db.Exec(`UPDATE servers SET version=?, datadir=CASE WHEN datadir='' THEN ? ELSE datadir END WHERE id=?`, version, datadir, id)
}

func (s *Store) DeleteServer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id)
	return err
}

func (s *Store) ServerInUse(id int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE server_id=?`, id).Scan(&n)
	return n, err
}

// ---- storages ----

const storageCols = `id,name,type,config,created_at,updated_at,last_test_at,last_test_ok,last_test_msg`

func (s *Store) scanStorage(row interface{ Scan(...any) error }) (*Storage, error) {
	var st Storage
	var cfg string
	var ca, ua int64
	var lt, lok sql.NullInt64
	if err := row.Scan(&st.ID, &st.Name, &st.Type, &cfg, &ca, &ua, &lt, &lok, &st.LastTestMsg); err != nil {
		return nil, notFound(err)
	}
	st.Config = s.decMap(cfg)
	st.CreatedAt, st.UpdatedAt = fromTS(ca), fromTS(ua)
	st.LastTestAt = fromNullTS(lt)
	if lok.Valid {
		ok := lok.Int64 == 1
		st.LastTestOK = &ok
	}
	return &st, nil
}

func (s *Store) ListStorages() ([]Storage, error) {
	rows, err := s.db.Query(`SELECT ` + storageCols + ` FROM storages ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Storage
	for rows.Next() {
		st, err := s.scanStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

func (s *Store) GetStorage(id int64) (*Storage, error) {
	return s.scanStorage(s.db.QueryRow(`SELECT `+storageCols+` FROM storages WHERE id=?`, id))
}

func (s *Store) CreateStorage(st *Storage) error {
	cfg, err := s.encMap(st.Config)
	if err != nil {
		return err
	}
	t := now()
	res, err := s.db.Exec(`INSERT INTO storages(name,type,config,created_at,updated_at) VALUES(?,?,?,?,?)`, st.Name, st.Type, cfg, t, t)
	if err != nil {
		return err
	}
	st.ID, _ = res.LastInsertId()
	st.CreatedAt, st.UpdatedAt = fromTS(t), fromTS(t)
	return nil
}

func (s *Store) UpdateStorage(st *Storage) error {
	cfg, err := s.encMap(st.Config)
	if err != nil {
		return err
	}
	t := now()
	_, err = s.db.Exec(`UPDATE storages SET name=?,type=?,config=?,updated_at=? WHERE id=?`, st.Name, st.Type, cfg, t, st.ID)
	st.UpdatedAt = fromTS(t)
	return err
}

// UpdateStorageConfig saves config without bumping updated_at (used for OAuth token refresh sync).
func (s *Store) UpdateStorageConfig(id int64, cfg map[string]string) error {
	c, err := s.encMap(cfg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE storages SET config=? WHERE id=?`, c, id)
	return err
}

func (s *Store) UpdateStorageTest(id int64, ok bool, msg string) {
	_, _ = s.db.Exec(`UPDATE storages SET last_test_at=?, last_test_ok=?, last_test_msg=? WHERE id=?`, now(), b2i(ok), msg, id)
}

func (s *Store) DeleteStorage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM storages WHERE id=?`, id)
	return err
}

func (s *Store) StorageInUse(id int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT j.name FROM job_targets t JOIN jobs j ON j.id=t.job_id WHERE t.storage_id=?`, id)
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

// ---- notification channels ----

func (s *Store) scanChannel(row interface{ Scan(...any) error }) (*NotifyChannel, error) {
	var c NotifyChannel
	var cfg string
	var en, os, of int
	var ca, ua int64
	if err := row.Scan(&c.ID, &c.Name, &c.Type, &cfg, &en, &os, &of, &ca, &ua); err != nil {
		return nil, notFound(err)
	}
	c.Config = s.decMap(cfg)
	c.Enabled, c.OnSuccess, c.OnFailure = en == 1, os == 1, of == 1
	c.CreatedAt, c.UpdatedAt = fromTS(ca), fromTS(ua)
	return &c, nil
}

const channelCols = `id,name,type,config,enabled,on_success,on_failure,created_at,updated_at`

func (s *Store) ListChannels() ([]NotifyChannel, error) {
	rows, err := s.db.Query(`SELECT ` + channelCols + ` FROM notify_channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyChannel
	for rows.Next() {
		c, err := s.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) GetChannel(id int64) (*NotifyChannel, error) {
	return s.scanChannel(s.db.QueryRow(`SELECT `+channelCols+` FROM notify_channels WHERE id=?`, id))
}

func (s *Store) SaveChannel(c *NotifyChannel) error {
	cfg, err := s.encMap(c.Config)
	if err != nil {
		return err
	}
	t := now()
	if c.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO notify_channels(name,type,config,enabled,on_success,on_failure,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			c.Name, c.Type, cfg, b2i(c.Enabled), b2i(c.OnSuccess), b2i(c.OnFailure), t, t)
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
		return nil
	}
	_, err = s.db.Exec(`UPDATE notify_channels SET name=?,type=?,config=?,enabled=?,on_success=?,on_failure=?,updated_at=? WHERE id=?`,
		c.Name, c.Type, cfg, b2i(c.Enabled), b2i(c.OnSuccess), b2i(c.OnFailure), t, c.ID)
	return err
}

func (s *Store) DeleteChannel(id int64) error {
	_, err := s.db.Exec(`DELETE FROM notify_channels WHERE id=?`, id)
	return err
}
