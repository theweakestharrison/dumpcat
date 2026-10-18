package store

import (
	"database/sql"
)

const jobCols = `j.id,j.uuid,j.name,j.server_id,COALESCE(s.name,''),j.method,j.db_mode,j.databases,j.exclude_tables,j.schedule,j.incr_schedule,
j.compression,j.compression_level,j.encrypt,j.passphrase,j.threads,j.options,j.retention,j.enabled,j.notify_success,j.notify_failure,j.timeout_minutes,j.created_at,j.updated_at`

func (s *Store) scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var dbs, excl, opts, ret, pass string
	var enc, en, ns, nf int
	var ca, ua int64
	if err := row.Scan(&j.ID, &j.UUID, &j.Name, &j.ServerID, &j.ServerName, &j.Method, &j.DBMode, &dbs, &excl, &j.Schedule, &j.IncrSchedule,
		&j.Compression, &j.CompressionLevel, &enc, &pass, &j.Threads, &opts, &ret, &en, &ns, &nf, &j.TimeoutMinutes, &ca, &ua); err != nil {
		return nil, notFound(err)
	}
	fromJSON(dbs, &j.Databases)
	fromJSON(excl, &j.ExcludeTables)
	fromJSON(opts, &j.Options)
	fromJSON(ret, &j.Retention)
	if j.Databases == nil {
		j.Databases = []string{}
	}
	if j.ExcludeTables == nil {
		j.ExcludeTables = []string{}
	}
	j.Encrypt = enc == 1
	j.Passphrase = s.dec(pass)
	j.HasPassphrase = j.Passphrase != ""
	j.Enabled, j.NotifySuccess, j.NotifyFailure = en == 1, ns == 1, nf == 1
	j.CreatedAt, j.UpdatedAt = fromTS(ca), fromTS(ua)
	return &j, nil
}

func (s *Store) loadTargets(jobs []*Job) error {
	if len(jobs) == 0 {
		return nil
	}
	idx := map[int64]*Job{}
	for _, j := range jobs {
		j.Targets = []JobTarget{}
		idx[j.ID] = j
	}
	rows, err := s.db.Query(`SELECT t.job_id,t.storage_id,st.name,st.type,t.retention FROM job_targets t JOIN storages st ON st.id=t.storage_id ORDER BY t.job_id,t.position`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jid int64
		var t JobTarget
		var ret string
		if err := rows.Scan(&jid, &t.StorageID, &t.StorageName, &t.StorageType, &ret); err != nil {
			return err
		}
		if ret != "" && ret != "null" {
			var r Retention
			fromJSON(ret, &r)
			t.Retention = &r
		}
		if j, ok := idx[jid]; ok {
			j.Targets = append(j.Targets, t)
		}
	}
	return rows.Err()
}

func (s *Store) ListJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT ` + jobCols + ` FROM jobs j LEFT JOIN servers s ON s.id=j.server_id ORDER BY j.name`)
	if err != nil {
		return nil, err
	}
	var ptrs []*Job
	for rows.Next() {
		j, err := s.scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ptrs = append(ptrs, j)
	}
	rows.Close()
	if err := s.loadTargets(ptrs); err != nil {
		return nil, err
	}
	out := make([]Job, len(ptrs))
	for i, p := range ptrs {
		out[i] = *p
	}
	return out, nil
}

func (s *Store) GetJob(id int64) (*Job, error) {
	j, err := s.scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs j LEFT JOIN servers s ON s.id=j.server_id WHERE j.id=?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.loadTargets([]*Job{j}); err != nil {
		return nil, err
	}
	return j, nil
}

func (s *Store) saveTargets(tx *sql.Tx, j *Job) error {
	if _, err := tx.Exec(`DELETE FROM job_targets WHERE job_id=?`, j.ID); err != nil {
		return err
	}
	for i, t := range j.Targets {
		ret := ""
		if t.Retention != nil {
			ret = toJSON(t.Retention)
		}
		if _, err := tx.Exec(`INSERT INTO job_targets(job_id,storage_id,retention,position) VALUES(?,?,?,?)`, j.ID, t.StorageID, ret, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateJob(j *Job) error {
	pass, err := s.enc(j.Passphrase)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.Exec(`INSERT INTO jobs(uuid,name,server_id,method,db_mode,databases,exclude_tables,schedule,incr_schedule,compression,compression_level,encrypt,passphrase,threads,options,retention,enabled,notify_success,notify_failure,timeout_minutes,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.UUID, j.Name, j.ServerID, j.Method, j.DBMode, toJSON(j.Databases), toJSON(j.ExcludeTables), j.Schedule, j.IncrSchedule, j.Compression, j.CompressionLevel,
		b2i(j.Encrypt), pass, j.Threads, toJSON(j.Options), toJSON(j.Retention), b2i(j.Enabled), b2i(j.NotifySuccess), b2i(j.NotifyFailure), j.TimeoutMinutes, t, t)
	if err != nil {
		return err
	}
	j.ID, _ = res.LastInsertId()
	if err := s.saveTargets(tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateJob(j *Job) error {
	pass, err := s.enc(j.Passphrase)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE jobs SET name=?,server_id=?,method=?,db_mode=?,databases=?,exclude_tables=?,schedule=?,incr_schedule=?,compression=?,compression_level=?,encrypt=?,passphrase=?,threads=?,options=?,retention=?,enabled=?,notify_success=?,notify_failure=?,timeout_minutes=?,updated_at=? WHERE id=?`,
		j.Name, j.ServerID, j.Method, j.DBMode, toJSON(j.Databases), toJSON(j.ExcludeTables), j.Schedule, j.IncrSchedule, j.Compression, j.CompressionLevel,
		b2i(j.Encrypt), pass, j.Threads, toJSON(j.Options), toJSON(j.Retention), b2i(j.Enabled), b2i(j.NotifySuccess), b2i(j.NotifyFailure), j.TimeoutMinutes, now(), j.ID)
	if err != nil {
		return err
	}
	if err := s.saveTargets(tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetJobEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE jobs SET enabled=?, updated_at=? WHERE id=?`, b2i(enabled), now(), id)
	return err
}

func (s *Store) DeleteJob(id int64) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE id=?`, id)
	return err
}
