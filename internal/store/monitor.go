package store

import (
	"database/sql"
	"time"
)

// ---- monitoring: metrics, freshness alerts, weekly digest ----

// StorageUse is what DumpCat keeps on a storage, as recorded in the catalog.
type StorageUse struct {
	StorageID   int64  `json:"storage_id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Backups     int    `json:"backups"`
	BackupBytes int64  `json:"backup_bytes"`
	BinlogBytes int64  `json:"binlog_bytes"`
}

func (u StorageUse) Bytes() int64 { return u.BackupBytes + u.BinlogBytes }

// StorageUsage sums the backups and archived binlogs stored on every storage.
func (s *Store) StorageUsage() ([]StorageUse, error) {
	backups := map[int64][2]int64{}
	rows, err := s.db.Query(`SELECT l.storage_id, COUNT(*), COALESCE(SUM(l.size_bytes),0) FROM run_locations l JOIN runs r ON r.id=l.run_id
		WHERE l.status='ok' AND l.storage_id IS NOT NULL AND r.status IN ('success','partial') GROUP BY l.storage_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, n, b int64
		if err := rows.Scan(&id, &n, &b); err != nil {
			rows.Close()
			return nil, err
		}
		backups[id] = [2]int64{n, b}
	}
	rows.Close()
	binlogs := map[int64]int64{}
	rows, err = s.db.Query(`SELECT bl.storage_id, COALESCE(SUM(b.stored_bytes),0) FROM binlog_locations bl JOIN binlog_files b ON b.id=bl.binlog_id
		WHERE bl.status='ok' AND bl.storage_id IS NOT NULL GROUP BY bl.storage_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, b int64
		if err := rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, err
		}
		binlogs[id] = b
	}
	rows.Close()
	sts, err := s.ListStorages()
	if err != nil {
		return nil, err
	}
	out := make([]StorageUse, 0, len(sts))
	for _, st := range sts {
		b := backups[st.ID]
		out = append(out, StorageUse{StorageID: st.ID, Name: st.Name, Type: st.Type, Backups: int(b[0]), BackupBytes: b[1], BinlogBytes: binlogs[st.ID]})
	}
	return out, nil
}

// RunsBetween returns the finished backups started in [from, to), oldest first.
func (s *Store) RunsBetween(from, to time.Time) ([]Run, error) {
	return s.queryRuns(` WHERE started_at>=? AND started_at<? AND status NOT IN ('queued','running')`, []any{from.Unix(), to.Unix()}, ` ORDER BY started_at, id`)
}

// LastFinishedRun returns the newest backup of a job that is not queued or running.
func (s *Store) LastFinishedRun(jobID int64) *Run {
	runs, err := s.queryRuns(` WHERE job_id=? AND status NOT IN ('queued','running')`, []any{jobID}, ` ORDER BY started_at DESC, id DESC LIMIT 1`)
	if err != nil || len(runs) == 0 {
		return nil
	}
	return &runs[0]
}

// LastFullBefore returns the newest successful full backup of a job started
// before t, also when retention has deleted it since (for growth figures).
func (s *Store) LastFullBefore(jobID int64, t time.Time) *Run {
	runs, err := s.queryRuns(` WHERE job_id=? AND type='full' AND started_at<? AND size_bytes>0 AND status IN ('success','partial','deleted')`,
		[]any{jobID, t.Unix()}, ` ORDER BY started_at DESC, id DESC LIMIT 1`)
	if err != nil || len(runs) == 0 {
		return nil
	}
	return &runs[0]
}

// LastVerifiedRun returns the backup of a job checked most recently.
func (s *Store) LastVerifiedRun(jobID int64) *Run {
	runs, err := s.queryRuns(` WHERE job_id=? AND verify_status IN ('ok','failed') AND verified_at IS NOT NULL`, []any{jobID}, ` ORDER BY verified_at DESC, id DESC LIMIT 1`)
	if err != nil || len(runs) == 0 {
		return nil
	}
	return &runs[0]
}

// RunCounts counts finished backups by status (the whole history kept).
func (s *Store) RunCounts() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM runs WHERE status NOT IN ('queued','running') GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var n sql.NullInt64
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n.Int64
	}
	return out, rows.Err()
}
