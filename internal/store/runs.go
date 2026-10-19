package store

import (
	"database/sql"
	"strings"
	"time"
)

const runCols = `id,uuid,job_id,job_name,job_uuid,job_slug,server_id,server_name,method,type,parent_id,base_id,parent_uuid,base_uuid,status,trigger,triggered_by,
started_at,finished_at,size_bytes,raw_bytes,files,databases,meta,error,compression,encrypted,passphrase,dir_name,verify_status,verified_at,verify_msg,
drill_status,drilled_at,drill_msg`

func (s *Store) scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var jid, sid, pid, bid, fin, ver, dr sql.NullInt64
	var st int64
	var files, dbs, meta, pass string
	var enc int
	if err := row.Scan(&r.ID, &r.UUID, &jid, &r.JobName, &r.JobUUID, &r.JobSlug, &sid, &r.ServerName, &r.Method, &r.Type, &pid, &bid, &r.ParentUUID, &r.BaseUUID,
		&r.Status, &r.Trigger, &r.TriggeredBy, &st, &fin, &r.SizeBytes, &r.RawBytes, &files, &dbs, &meta, &r.Error, &r.Compression, &enc, &pass,
		&r.DirName, &r.VerifyStatus, &ver, &r.VerifyMsg, &r.DrillStatus, &dr, &r.DrillMsg); err != nil {
		return nil, notFound(err)
	}
	r.JobID, r.ServerID, r.ParentID, r.BaseID = fromNullID(jid), fromNullID(sid), fromNullID(pid), fromNullID(bid)
	r.StartedAt = fromTS(st)
	r.FinishedAt = fromNullTS(fin)
	r.VerifiedAt = fromNullTS(ver)
	r.DrilledAt = fromNullTS(dr)
	fromJSON(files, &r.Files)
	fromJSON(dbs, &r.Databases)
	fromJSON(meta, &r.Meta)
	if r.Files == nil {
		r.Files = []BackupFile{}
	}
	if r.Databases == nil {
		r.Databases = []DBEntry{}
	}
	if r.Meta == nil {
		r.Meta = map[string]any{}
	}
	r.Encrypted = enc == 1
	r.Passphrase = s.dec(pass)
	r.HasPassphrase = r.Passphrase != ""
	r.Locations = []RunLocation{}
	return &r, nil
}

func (s *Store) CreateRun(r *Run) error {
	pass, err := s.enc(r.Passphrase)
	if err != nil {
		return err
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO runs(uuid,job_id,job_name,job_uuid,job_slug,server_id,server_name,method,type,parent_id,base_id,parent_uuid,base_uuid,status,trigger,triggered_by,
		started_at,finished_at,size_bytes,raw_bytes,files,databases,meta,error,compression,encrypted,passphrase,dir_name,verify_status,verified_at,verify_msg)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.UUID, nullID(r.JobID), r.JobName, r.JobUUID, r.JobSlug, nullID(r.ServerID), r.ServerName, r.Method, r.Type, nullID(r.ParentID), nullID(r.BaseID), r.ParentUUID, r.BaseUUID,
		r.Status, r.Trigger, r.TriggeredBy, r.StartedAt.Unix(), nullTS(r.FinishedAt), r.SizeBytes, r.RawBytes, toJSON(r.Files), toJSON(r.Databases), toJSON(r.Meta),
		r.Error, r.Compression, b2i(r.Encrypted), pass, r.DirName, r.VerifyStatus, nullTS(r.VerifiedAt), r.VerifyMsg)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateRun(r *Run) error {
	pass, err := s.enc(r.Passphrase)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE runs SET job_id=?,job_name=?,server_id=?,server_name=?,type=?,parent_id=?,base_id=?,parent_uuid=?,base_uuid=?,status=?,started_at=?,finished_at=?,
		size_bytes=?,raw_bytes=?,files=?,databases=?,meta=?,error=?,compression=?,encrypted=?,passphrase=?,dir_name=?,verify_status=?,verified_at=?,verify_msg=? WHERE id=?`,
		nullID(r.JobID), r.JobName, nullID(r.ServerID), r.ServerName, r.Type, nullID(r.ParentID), nullID(r.BaseID), r.ParentUUID, r.BaseUUID, r.Status, r.StartedAt.Unix(), nullTS(r.FinishedAt),
		r.SizeBytes, r.RawBytes, toJSON(r.Files), toJSON(r.Databases), toJSON(r.Meta), r.Error, r.Compression, b2i(r.Encrypted), pass, r.DirName,
		r.VerifyStatus, nullTS(r.VerifiedAt), r.VerifyMsg, r.ID)
	return err
}

func (s *Store) SetRunStatus(id int64, status, errMsg string) {
	_, _ = s.db.Exec(`UPDATE runs SET status=?, error=CASE WHEN ?<>'' THEN ? ELSE error END WHERE id=?`, status, errMsg, errMsg, id)
}

func (s *Store) SetRunVerify(id int64, status, msg string) {
	_, _ = s.db.Exec(`UPDATE runs SET verify_status=?, verify_msg=?, verified_at=? WHERE id=?`, status, msg, now(), id)
}

// SetRunDrill records the result of the last restore drill of a backup.
func (s *Store) SetRunDrill(id int64, status, msg string) {
	_, _ = s.db.Exec(`UPDATE runs SET drill_status=?, drill_msg=?, drilled_at=? WHERE id=?`, status, msg, now(), id)
}

// SetRunTables stores the rows per table counted in a logical backup. They
// are kept apart from the run so that lists of backups stay small.
func (s *Store) SetRunTables(id int64, t TableStats) error {
	v := ""
	if t != nil {
		v = toJSON(t)
	}
	_, err := s.db.Exec(`UPDATE runs SET table_stats=? WHERE id=?`, v, id)
	return err
}

// RunTables returns the rows per table of a backup (nil: not recorded).
func (s *Store) RunTables(id int64) (TableStats, error) {
	var v string
	if err := s.db.QueryRow(`SELECT table_stats FROM runs WHERE id=?`, id).Scan(&v); err != nil {
		return nil, notFound(err)
	}
	if v == "" {
		return nil, nil
	}
	var t TableStats
	fromJSON(v, &t)
	return t, nil
}

func (s *Store) loadLocations(runs []*Run) error {
	if len(runs) == 0 {
		return nil
	}
	idx := map[int64]*Run{}
	ids := make([]string, 0, len(runs))
	args := make([]any, 0, len(runs))
	for _, r := range runs {
		idx[r.ID] = r
		ids = append(ids, "?")
		args = append(args, r.ID)
	}
	rows, err := s.db.Query(`SELECT id,run_id,storage_id,storage_name,storage_type,path,status,error,size_bytes,created_at,updated_at FROM run_locations WHERE run_id IN (`+strings.Join(ids, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l RunLocation
		var sid sql.NullInt64
		var ca, ua int64
		if err := rows.Scan(&l.ID, &l.RunID, &sid, &l.StorageName, &l.StorageType, &l.Path, &l.Status, &l.Error, &l.SizeBytes, &ca, &ua); err != nil {
			return err
		}
		l.StorageID = fromNullID(sid)
		l.CreatedAt, l.UpdatedAt = fromTS(ca), fromTS(ua)
		if r, ok := idx[l.RunID]; ok {
			r.Locations = append(r.Locations, l)
		}
	}
	return rows.Err()
}

func (s *Store) GetRun(id int64) (*Run, error) {
	r, err := s.scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return r, s.loadLocations([]*Run{r})
}

func (s *Store) GetRunByUUID(uuid string) (*Run, error) {
	r, err := s.scanRun(s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE uuid=?`, uuid))
	if err != nil {
		return nil, err
	}
	return r, s.loadLocations([]*Run{r})
}

type RunFilter struct {
	JobID    int64
	Status   string
	Method   string
	Search   string
	Since    time.Time
	Limit    int
	Offset   int
	OnlyLive bool // exclude deleted runs
}

func (s *Store) queryRuns(where string, args []any, tail string) ([]Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs`+where+tail, args...)
	if err != nil {
		return nil, err
	}
	var ptrs []*Run
	for rows.Next() {
		r, err := s.scanRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ptrs = append(ptrs, r)
	}
	rows.Close()
	if err := s.loadLocations(ptrs); err != nil {
		return nil, err
	}
	out := make([]Run, len(ptrs))
	for i, p := range ptrs {
		out[i] = *p
	}
	return out, nil
}

func (s *Store) ListRuns(f RunFilter) ([]Run, int, error) {
	var conds []string
	var args []any
	if f.JobID > 0 {
		conds = append(conds, "job_id=?")
		args = append(args, f.JobID)
	}
	if f.Status != "" {
		conds = append(conds, "status=?")
		args = append(args, f.Status)
	}
	if f.Method != "" {
		conds = append(conds, "method=?")
		args = append(args, f.Method)
	}
	if f.Search != "" {
		conds = append(conds, "(job_name LIKE ? OR server_name LIKE ? OR uuid LIKE ? OR dir_name LIKE ?)")
		p := "%" + f.Search + "%"
		args = append(args, p, p, p, p)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "started_at>=?")
		args = append(args, f.Since.Unix())
	}
	if f.OnlyLive {
		conds = append(conds, "status<>'deleted'")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 50
	}
	runs, err := s.queryRuns(where, append(args, f.Limit, f.Offset), ` ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`)
	return runs, total, err
}

// LastRun returns the most recent run of a job (any status) or nil.
func (s *Store) LastRun(jobID int64) *Run {
	runs, err := s.queryRuns(` WHERE job_id=? AND status NOT IN ('queued')`, []any{jobID}, ` ORDER BY started_at DESC, id DESC LIMIT 1`)
	if err != nil || len(runs) == 0 {
		return nil
	}
	return &runs[0]
}

// LastSuccessfulRun returns the latest successful (or partial) run of a job
// optionally restricted to method.
func (s *Store) LastSuccessfulRun(jobID int64, method string) *Run {
	runs, err := s.queryRuns(` WHERE job_id=? AND method=? AND status IN ('success','partial')`, []any{jobID, method}, ` ORDER BY started_at DESC, id DESC LIMIT 1`)
	if err != nil || len(runs) == 0 {
		return nil
	}
	return &runs[0]
}

// RetentionCandidates returns successful runs for a job (newest first).
func (s *Store) RetentionCandidates(jobID int64) ([]Run, error) {
	return s.queryRuns(` WHERE job_id=? AND status IN ('success','partial')`, []any{jobID}, ` ORDER BY started_at DESC, id DESC`)
}

// RunsByJobUUID returns the successful runs of a job (also after the job was
// deleted or the runs were imported), newest first.
func (s *Store) RunsByJobUUID(uuid string) ([]Run, error) {
	return s.queryRuns(` WHERE job_uuid=? AND status IN ('success','partial')`, []any{uuid}, ` ORDER BY started_at DESC, id DESC`)
}

// ChainOf returns the full backup with id baseID plus all incrementals based on it, oldest first.
func (s *Store) ChainOf(baseID int64) ([]Run, error) {
	return s.queryRuns(` WHERE (id=? OR base_id=?) AND status IN ('success','partial')`, []any{baseID, baseID}, ` ORDER BY started_at ASC, id ASC`)
}

// Dependents returns live runs that are based on or children of run id.
func (s *Store) Dependents(id int64) ([]Run, error) {
	return s.queryRuns(` WHERE (parent_id=? OR base_id=?) AND id<>? AND status NOT IN ('deleted','failed','canceled')`, []any{id, id, id}, ` ORDER BY started_at ASC`)
}

func (s *Store) DeleteRunRow(id int64) error {
	_, err := s.db.Exec(`DELETE FROM runs WHERE id=?`, id)
	return err
}

// MarkInterrupted settles tasks left running by a crash/restart. It returns
// the locations that were still being written (their data is incomplete).
func (s *Store) MarkInterrupted() []RunLocation {
	var partial []RunLocation
	if rows, err := s.db.Query(`SELECT l.id,l.run_id,l.storage_id,l.path FROM run_locations l JOIN runs r ON r.id=l.run_id WHERE l.status='uploading'`); err == nil {
		for rows.Next() {
			var l RunLocation
			var sid sql.NullInt64
			if rows.Scan(&l.ID, &l.RunID, &sid, &l.Path) == nil {
				l.StorageID = fromNullID(sid)
				partial = append(partial, l)
			}
		}
		rows.Close()
	}
	t := now()
	msg := "Interrupted by an application restart"
	_, _ = s.db.Exec(`UPDATE run_locations SET status='failed', error='interrupted', updated_at=? WHERE status='uploading'`, t)
	// a run whose backup already reached at least one storage stays usable
	_, _ = s.db.Exec(`UPDATE runs SET status = CASE WHEN EXISTS(SELECT 1 FROM run_locations l WHERE l.run_id=runs.id AND l.status='ok') THEN 'partial' ELSE 'failed' END,
		error=?, finished_at=? WHERE status IN ('queued','running')`, msg, t)
	_, _ = s.db.Exec(`UPDATE runs SET verify_status='', verify_msg='verification interrupted' WHERE verify_status='running'`)
	_, _ = s.db.Exec(`UPDATE restores SET status='failed', error=?, finished_at=? WHERE status IN ('queued','running')`, msg, t)
	return partial
}

// OldFinishedRuns returns ids of failed/canceled/deleted runs older than before.
func (s *Store) OldFinishedRuns(before time.Time) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM runs WHERE status IN ('failed','canceled','deleted') AND started_at < ?`, before.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---- locations ----

func (s *Store) AddLocation(l *RunLocation) error {
	t := now()
	res, err := s.db.Exec(`INSERT INTO run_locations(run_id,storage_id,storage_name,storage_type,path,status,error,size_bytes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		l.RunID, nullID(l.StorageID), l.StorageName, l.StorageType, l.Path, l.Status, l.Error, l.SizeBytes, t, t)
	if err != nil {
		return err
	}
	l.ID, _ = res.LastInsertId()
	l.CreatedAt, l.UpdatedAt = fromTS(t), fromTS(t)
	return nil
}

func (s *Store) UpdateLocation(l *RunLocation) error {
	_, err := s.db.Exec(`UPDATE run_locations SET status=?, error=?, size_bytes=?, path=?, updated_at=? WHERE id=?`, l.Status, l.Error, l.SizeBytes, l.Path, now(), l.ID)
	return err
}

func (s *Store) DeleteLocation(id int64) error {
	_, err := s.db.Exec(`DELETE FROM run_locations WHERE id=?`, id)
	return err
}

// FailUploadingLocations marks unfinished locations of a run as failed.
func (s *Store) FailUploadingLocations(runID int64, msg string) {
	_, _ = s.db.Exec(`UPDATE run_locations SET status='failed', error=?, updated_at=? WHERE run_id=? AND status='uploading'`, msg, now(), runID)
}

func (s *Store) LocationExists(runID, storageID int64) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM run_locations WHERE run_id=? AND storage_id=? AND status='ok'`, runID, storageID).Scan(&n)
	return n > 0
}

// ---- dashboard ----

type Stats struct {
	Jobs          int   `json:"jobs"`
	JobsEnabled   int   `json:"jobs_enabled"`
	Servers       int   `json:"servers"`
	Storages      int   `json:"storages"`
	Runs24h       int   `json:"runs_24h"`
	Success24h    int   `json:"success_24h"`
	Failed24h     int   `json:"failed_24h"`
	Backups       int   `json:"backups"`
	StoredBytes   int64 `json:"stored_bytes"`
	LastSuccessAt int64 `json:"last_success_at"`
}

func (s *Store) Stats() Stats {
	var st Stats
	since := time.Now().Add(-24 * time.Hour).Unix()
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM jobs`).Scan(&st.Jobs, &st.JobsEnabled)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&st.Servers)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM storages`).Scan(&st.Storages)
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status IN ('success')),0), COALESCE(SUM(status IN ('failed','partial')),0) FROM runs WHERE started_at>=? AND status NOT IN ('queued','running')`, since).
		Scan(&st.Runs24h, &st.Success24h, &st.Failed24h)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE status IN ('success','partial')`).Scan(&st.Backups)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(l.size_bytes),0) FROM run_locations l JOIN runs r ON r.id=l.run_id WHERE l.status='ok' AND r.status IN ('success','partial')`).Scan(&st.StoredBytes)
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(finished_at),0) FROM runs WHERE status='success'`).Scan(&st.LastSuccessAt)
	return st
}

type DayStat struct {
	Day     string `json:"day"`
	Success int    `json:"success"`
	Failed  int    `json:"failed"`
	Bytes   int64  `json:"bytes"`
}

func (s *Store) DailyStats(days int, loc *time.Location) []DayStat {
	if loc == nil {
		loc = time.Local
	}
	start := time.Now().In(loc).AddDate(0, 0, -(days - 1))
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	out := make([]DayStat, days)
	idx := map[string]int{}
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		out[i].Day = d
		idx[d] = i
	}
	rows, err := s.db.Query(`SELECT started_at, status, size_bytes FROM runs WHERE started_at>=? AND status NOT IN ('queued','running')`, start.Unix())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, sz int64
		var status string
		if rows.Scan(&st, &status, &sz) != nil {
			continue
		}
		d := time.Unix(st, 0).In(loc).Format("2006-01-02")
		i, ok := idx[d]
		if !ok {
			continue
		}
		switch status {
		case StatusSuccess, StatusDeleted:
			out[i].Success++
			out[i].Bytes += sz
		case StatusPartial:
			out[i].Failed++
			out[i].Bytes += sz
		case StatusFailed:
			out[i].Failed++
		}
	}
	return out
}

// ---- restores ----

const restoreCols = `id,run_id,run_uuid,job_name,kind,target_server_id,target_server_name,options,status,started_at,finished_at,error,user_id,username`

func scanRestore(row interface{ Scan(...any) error }) (*Restore, error) {
	var r Restore
	var rid, tsid, uid, fin sql.NullInt64
	var st int64
	var opts string
	if err := row.Scan(&r.ID, &rid, &r.RunUUID, &r.JobName, &r.Kind, &tsid, &r.TargetServerName, &opts, &r.Status, &st, &fin, &r.Error, &uid, &r.Username); err != nil {
		return nil, notFound(err)
	}
	r.RunID, r.TargetServerID, r.UserID = fromNullID(rid), fromNullID(tsid), fromNullID(uid)
	r.StartedAt = fromTS(st)
	r.FinishedAt = fromNullTS(fin)
	if opts == "" {
		opts = "{}"
	}
	r.Options = []byte(opts)
	return &r, nil
}

func (s *Store) CreateRestore(r *Restore) error {
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	opts := string(r.Options)
	if opts == "" {
		opts = "{}"
	}
	res, err := s.db.Exec(`INSERT INTO restores(run_id,run_uuid,job_name,kind,target_server_id,target_server_name,options,status,started_at,error,user_id,username) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullID(r.RunID), r.RunUUID, r.JobName, r.Kind, nullID(r.TargetServerID), r.TargetServerName, opts, r.Status, r.StartedAt.Unix(), r.Error, nullID(r.UserID), r.Username)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) FinishRestore(id int64, status, errMsg string) {
	_, _ = s.db.Exec(`UPDATE restores SET status=?, error=?, finished_at=? WHERE id=?`, status, errMsg, now(), id)
}

func (s *Store) SetRestoreStatus(id int64, status string) {
	_, _ = s.db.Exec(`UPDATE restores SET status=? WHERE id=?`, status, id)
}

func (s *Store) GetRestore(id int64) (*Restore, error) {
	return scanRestore(s.db.QueryRow(`SELECT `+restoreCols+` FROM restores WHERE id=?`, id))
}

func (s *Store) ListRestores(limit, offset int) ([]Restore, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM restores`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+restoreCols+` FROM restores ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Restore
	for rows.Next() {
		r, err := scanRestore(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}
