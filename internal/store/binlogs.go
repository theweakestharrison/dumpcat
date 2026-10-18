package store

import (
	"database/sql"
	"strings"
	"time"
)

const binlogCols = `id,job_id,job_uuid,job_name,server_id,server_name,file_name,seq,first_ts,last_ts,size_bytes,stored_bytes,sha256,raw_sha256,
compression,encrypted,passphrase,path,created_at`

func (s *Store) scanBinlog(row interface{ Scan(...any) error }) (*BinlogFile, error) {
	var b BinlogFile
	var jid, sid sql.NullInt64
	var first, last, created int64
	var enc int
	var pass string
	if err := row.Scan(&b.ID, &jid, &b.JobUUID, &b.JobName, &sid, &b.ServerName, &b.FileName, &b.Seq, &first, &last, &b.SizeBytes, &b.StoredBytes,
		&b.SHA256, &b.RawSHA256, &b.Compression, &enc, &pass, &b.Path, &created); err != nil {
		return nil, notFound(err)
	}
	b.JobID, b.ServerID = fromNullID(jid), fromNullID(sid)
	b.FirstTS, b.LastTS, b.CreatedAt = fromTS(first), fromTS(last), fromTS(created)
	b.Encrypted = enc == 1
	b.Passphrase = s.dec(pass)
	b.Locations = []BinlogLocation{}
	return &b, nil
}

func (s *Store) queryBinlogs(where string, args []any, tail string) ([]BinlogFile, error) {
	rows, err := s.db.Query(`SELECT `+binlogCols+` FROM binlog_files`+where+tail, args...)
	if err != nil {
		return nil, err
	}
	var out []BinlogFile
	for rows.Next() {
		b, err := s.scanBinlog(rows)
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
	return out, s.loadBinlogLocations(out)
}

func (s *Store) loadBinlogLocations(files []BinlogFile) error {
	if len(files) == 0 {
		return nil
	}
	idx := map[int64]*BinlogFile{}
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
			var l BinlogLocation
			var sid sql.NullInt64
			var ua int64
			if err := rows.Scan(&l.ID, &l.BinlogID, &sid, &l.StorageName, &l.StorageType, &l.Status, &l.Error, &ua); err != nil {
				rows.Close()
				return err
			}
			l.StorageID = fromNullID(sid)
			l.UpdatedAt = fromTS(ua)
			if f, ok := idx[l.BinlogID]; ok {
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

// AddBinlogFile records an archived file together with its locations.
func (s *Store) AddBinlogFile(b *BinlogFile) error {
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
	res, err := tx.Exec(`INSERT INTO binlog_files(job_id,job_uuid,job_name,server_id,server_name,file_name,seq,first_ts,last_ts,size_bytes,stored_bytes,sha256,raw_sha256,
		compression,encrypted,passphrase,path,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullID(b.JobID), b.JobUUID, b.JobName, nullID(b.ServerID), b.ServerName, b.FileName, b.Seq, b.FirstTS.Unix(), b.LastTS.Unix(), b.SizeBytes, b.StoredBytes,
		b.SHA256, b.RawSHA256, b.Compression, b2i(b.Encrypted), pass, b.Path, t)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	b.CreatedAt = fromTS(t)
	for i := range b.Locations {
		l := &b.Locations[i]
		l.BinlogID = b.ID
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

// SaveBinlogLocation inserts or updates the location of a file on a storage.
func (s *Store) SaveBinlogLocation(l *BinlogLocation) error {
	t := now()
	l.UpdatedAt = fromTS(t)
	if l.ID > 0 {
		_, err := s.db.Exec(`UPDATE binlog_locations SET status=?, error=?, updated_at=? WHERE id=?`, l.Status, l.Error, t, l.ID)
		return err
	}
	res, err := s.db.Exec(`INSERT INTO binlog_locations(binlog_id,storage_id,storage_name,storage_type,status,error,updated_at) VALUES(?,?,?,?,?,?,?)`,
		l.BinlogID, nullID(l.StorageID), l.StorageName, l.StorageType, l.Status, l.Error, t)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) DeleteBinlogLocation(id int64) error {
	_, err := s.db.Exec(`DELETE FROM binlog_locations WHERE id=?`, id)
	return err
}

// DeleteBinlogIfOrphan removes a file row once no storage holds it any more.
func (s *Store) DeleteBinlogIfOrphan(id int64) {
	_, _ = s.db.Exec(`DELETE FROM binlog_files WHERE id=? AND NOT EXISTS(SELECT 1 FROM binlog_locations WHERE binlog_id=? AND status='ok')`, id, id)
}

// ListBinlogFiles returns every archived file of a job in archive order.
func (s *Store) ListBinlogFiles(jobUUID string) ([]BinlogFile, error) {
	return s.queryBinlogs(` WHERE job_uuid=?`, []any{jobUUID}, ` ORDER BY id ASC`)
}

// ListBinlogPage returns archived files of a job, most recently archived first.
func (s *Store) ListBinlogPage(jobUUID string, limit, offset int) ([]BinlogFile, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM binlog_files WHERE job_uuid=?`, jobUUID).Scan(&total); err != nil {
		return nil, 0, err
	}
	out, err := s.queryBinlogs(` WHERE job_uuid=?`, []any{jobUUID, limit, offset}, ` ORDER BY id DESC LIMIT ? OFFSET ?`)
	return out, total, err
}

// FindBinlogFile looks a file up by name and creation time (a RESET MASTER
// restarts the numbering, so the name alone is not unique).
func (s *Store) FindBinlogFile(jobUUID, name string, firstTS time.Time) (*BinlogFile, error) {
	out, err := s.queryBinlogs(` WHERE job_uuid=? AND file_name=? AND first_ts=?`, []any{jobUUID, name, firstTS.Unix()}, ` ORDER BY id DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// LastBinlogFile returns the most recently archived file of a job.
func (s *Store) LastBinlogFile(jobUUID string) *BinlogFile {
	out, err := s.queryBinlogs(` WHERE job_uuid=?`, []any{jobUUID}, ` ORDER BY id DESC LIMIT 1`)
	if err != nil || len(out) == 0 {
		return nil
	}
	return &out[0]
}

func (s *Store) BinlogPathUsed(path string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM binlog_files WHERE path=?`, path).Scan(&n)
	return n > 0
}

type BinlogStats struct {
	Files       int        `json:"files"`
	RawBytes    int64      `json:"raw_bytes"`
	StoredBytes int64      `json:"stored_bytes"`
	First       *time.Time `json:"first"`
	Last        *time.Time `json:"last"`
}

func (s *Store) BinlogStats(jobUUID string) BinlogStats {
	var st BinlogStats
	var first, last sql.NullInt64
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size_bytes),0), COALESCE(SUM(stored_bytes),0), MIN(first_ts), MAX(last_ts) FROM binlog_files WHERE job_uuid=?`, jobUUID).
		Scan(&st.Files, &st.RawBytes, &st.StoredBytes, &first, &last)
	st.First, st.Last = fromNullTS(first), fromNullTS(last)
	return st
}
