package store

import (
	"database/sql"
	"strings"
	"time"
)

const logFileCols = `id,job_id,job_uuid,job_name,server_id,server_name,file_name,seq,first_ts,last_ts,size_bytes,stored_bytes,sha256,raw_sha256,
compression,encrypted,passphrase,path,created_at,meta`

// optTS reads a time that may be absent (0: the file holds no timed event).
func optTS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return fromTS(v)
}

func tsOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Store) scanLogFile(row interface{ Scan(...any) error }) (*LogFile, error) {
	var b LogFile
	var jid, sid sql.NullInt64
	var first, last, created int64
	var enc int
	var pass, meta string
	if err := row.Scan(&b.ID, &jid, &b.JobUUID, &b.JobName, &sid, &b.ServerName, &b.FileName, &b.Seq, &first, &last, &b.SizeBytes, &b.StoredBytes,
		&b.SHA256, &b.RawSHA256, &b.Compression, &enc, &pass, &b.Path, &created, &meta); err != nil {
		return nil, notFound(err)
	}
	b.JobID, b.ServerID = fromNullID(jid), fromNullID(sid)
	b.FirstTS, b.LastTS, b.CreatedAt = optTS(first), optTS(last), fromTS(created)
	fromJSON(meta, &b.Meta)
	b.Encrypted = enc == 1
	b.Passphrase = s.dec(pass)
	b.Locations = []LogLocation{}
	return &b, nil
}

func (s *Store) queryLogFiles(where string, args []any, tail string) ([]LogFile, error) {
	rows, err := s.db.Query(`SELECT `+logFileCols+` FROM binlog_files`+where+tail, args...)
	if err != nil {
		return nil, err
	}
	var out []LogFile
	for rows.Next() {
		b, err := s.scanLogFile(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.loadLogLocations(out)
}

func (s *Store) loadLogLocations(files []LogFile) error {
	if len(files) == 0 {
		return nil
	}
	idx := map[int64]*LogFile{}
	for i := range files {
		idx[files[i].ID] = &files[i]
	}
	// one query per chunk keeps the variable count well below SQLite's limit
	for start := 0; start < len(files); start += 500 {
		end := min(start+500, len(files))
		ph := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for _, f := range files[start:end] {
			ph = append(ph, "?")
			args = append(args, f.ID)
		}
		rows, err := s.db.Query(`SELECT id,binlog_id,storage_id,storage_name,storage_type,status,error,updated_at FROM binlog_locations WHERE binlog_id IN (`+strings.Join(ph, ",")+`) ORDER BY id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var l LogLocation
			var sid sql.NullInt64
			var ua int64
			if err := rows.Scan(&l.ID, &l.FileID, &sid, &l.StorageName, &l.StorageType, &l.Status, &l.Error, &ua); err != nil {
				rows.Close()
				return err
			}
			l.StorageID = fromNullID(sid)
			l.UpdatedAt = fromTS(ua)
			if f, ok := idx[l.FileID]; ok {
				f.Locations = append(f.Locations, l)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// AddLogFile records an archived file together with its locations.
func (s *Store) AddLogFile(b *LogFile) error {
	pass, err := s.enc(b.Passphrase)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	meta := ""
	if len(b.Meta) > 0 {
		meta = toJSON(b.Meta)
	}
	res, err := tx.Exec(`INSERT INTO binlog_files(job_id,job_uuid,job_name,server_id,server_name,file_name,seq,first_ts,last_ts,size_bytes,stored_bytes,sha256,raw_sha256,
		compression,encrypted,passphrase,path,created_at,meta) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullID(b.JobID), b.JobUUID, b.JobName, nullID(b.ServerID), b.ServerName, b.FileName, b.Seq, tsOrZero(b.FirstTS), tsOrZero(b.LastTS), b.SizeBytes, b.StoredBytes,
		b.SHA256, b.RawSHA256, b.Compression, b2i(b.Encrypted), pass, b.Path, t, meta)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	b.CreatedAt = fromTS(t)
	for i := range b.Locations {
		l := &b.Locations[i]
		l.FileID = b.ID
		res, err := tx.Exec(`INSERT INTO binlog_locations(binlog_id,storage_id,storage_name,storage_type,status,error,updated_at) VALUES(?,?,?,?,?,?,?)`,
			b.ID, nullID(l.StorageID), l.StorageName, l.StorageType, l.Status, l.Error, t)
		if err != nil {
			return err
		}
		l.ID, _ = res.LastInsertId()
		l.UpdatedAt = fromTS(t)
	}
	return tx.Commit()
}

// SaveLogLocation inserts or updates the location of a file on a storage.
func (s *Store) SaveLogLocation(l *LogLocation) error {
	t := now()
	l.UpdatedAt = fromTS(t)
	if l.ID > 0 {
		_, err := s.db.Exec(`UPDATE binlog_locations SET status=?, error=?, updated_at=? WHERE id=?`, l.Status, l.Error, t, l.ID)
		return err
	}
	res, err := s.db.Exec(`INSERT INTO binlog_locations(binlog_id,storage_id,storage_name,storage_type,status,error,updated_at) VALUES(?,?,?,?,?,?,?)`,
		l.FileID, nullID(l.StorageID), l.StorageName, l.StorageType, l.Status, l.Error, t)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) DeleteLogLocation(id int64) error {
	_, err := s.db.Exec(`DELETE FROM binlog_locations WHERE id=?`, id)
	return err
}

// DeleteLogIfOrphan removes a file row once no storage holds it any more.
func (s *Store) DeleteLogIfOrphan(id int64) {
	_, _ = s.db.Exec(`DELETE FROM binlog_files WHERE id=? AND NOT EXISTS(SELECT 1 FROM binlog_locations WHERE binlog_id=? AND status='ok')`, id, id)
}

// ListLogFiles returns every archived file of a job in archive order.
func (s *Store) ListLogFiles(jobUUID string) ([]LogFile, error) {
	return s.queryLogFiles(` WHERE job_uuid=?`, []any{jobUUID}, ` ORDER BY id ASC`)
}

// ListLogPage returns archived files of a job, most recently archived first.
func (s *Store) ListLogPage(jobUUID string, limit, offset int) ([]LogFile, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM binlog_files WHERE job_uuid=?`, jobUUID).Scan(&total); err != nil {
		return nil, 0, err
	}
	out, err := s.queryLogFiles(` WHERE job_uuid=?`, []any{jobUUID, limit, offset}, ` ORDER BY id DESC LIMIT ? OFFSET ?`)
	return out, total, err
}

// FindLogFile looks a file up by name and first event (a RESET MASTER
// restarts the numbering, so the name alone is not unique).
func (s *Store) FindLogFile(jobUUID, name string, firstTS time.Time) (*LogFile, error) {
	out, err := s.queryLogFiles(` WHERE job_uuid=? AND file_name=? AND first_ts=?`, []any{jobUUID, name, tsOrZero(firstTS)}, ` ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// LastLogFile returns the most recently archived file of a job.
func (s *Store) LastLogFile(jobUUID string) *LogFile {
	out, err := s.queryLogFiles(` WHERE job_uuid=?`, []any{jobUUID}, ` ORDER BY id DESC LIMIT 1`)
	if err != nil || len(out) == 0 {
		return nil
	}
	return &out[0]
}

func (s *Store) LogPathUsed(path string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM binlog_files WHERE path=?`, path).Scan(&n)
	return n > 0
}

type LogStats struct {
	Files       int        `json:"files"`
	RawBytes    int64      `json:"raw_bytes"`
	StoredBytes int64      `json:"stored_bytes"`
	First       *time.Time `json:"first"`
	Last        *time.Time `json:"last"`
}

func (s *Store) LogStats(jobUUID string) LogStats {
	var st LogStats
	var first, last sql.NullInt64
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes),0), COALESCE(SUM(stored_bytes),0), MIN(NULLIF(first_ts,0)), MAX(NULLIF(last_ts,0)) FROM binlog_files WHERE job_uuid=?`, jobUUID).
		Scan(&st.Files, &st.RawBytes, &st.StoredBytes, &first, &last)
	st.First, st.Last = fromNullTS(first), fromNullTS(last)
	return st
}
