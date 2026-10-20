package store

import (
	"database/sql"
	"strings"
	"time"
)

// TableStats holds the rows of every table of a logical backup, per
// database, as counted from the dump itself.
type TableStats map[string]map[string]int64

// Drill restores a job's latest backup into a sandbox server under temporary
// database names, checks the result and drops the copies again, on a
// schedule or on demand: proof that the backups really restore.
type Drill struct {
	ID               int64            `json:"id"`
	JobID            int64            `json:"job_id"`
	JobName          string           `json:"job_name"`
	Name             string           `json:"name"`
	TargetServerID   int64            `json:"target_server_id"`
	TargetServerName string           `json:"target_server_name"`
	Schedule         string           `json:"schedule"`
	Enabled          bool             `json:"enabled"`
	Databases        []string         `json:"databases"` // empty: every database of the backup
	Assertions       []DrillAssertion `json:"assertions"`
	Options          DrillOptions     `json:"options"`
	NotifySuccess    bool             `json:"notify_success"`
	NotifyFailure    bool             `json:"notify_failure"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

// DrillAssertion is a query run against the restored copy: it passes when
// the first column of its first row is true (not NULL, 0 or empty).
type DrillAssertion struct {
	Name     string `json:"name"`
	Database string `json:"database"` // runs inside the copy of this database ("" = none)
	SQL      string `json:"sql"`
}

type DrillOptions struct {
	Threads       int    `json:"threads"`
	NoBinlog      bool   `json:"no_binlog"`       // keep the drill out of the sandbox's binary log
	MaxRTOMinutes int    `json:"max_rto_minutes"` // fail when the restore takes longer (0 = no limit)
	RowCounts     string `json:"row_counts"`      // "" (exact, COUNT(*)) or "estimate" (the server's statistics)
}

// DrillRowsEstimate compares the server's row estimates (after ANALYZE)
// instead of counting every row: for tables too big to count.
const DrillRowsEstimate = "estimate"

const (
	DrillPassed = "passed"
	DrillFailed = "failed"

	CheckOK      = "ok"
	CheckFailed  = "failed"
	CheckWarning = "warning"
	CheckSkipped = "skipped"
)

// DrillDB maps a database of the backup to its temporary copy.
type DrillDB struct {
	Name   string `json:"name"`
	Temp   string `json:"temp"`
	Tables int    `json:"tables"`
	Rows   int64  `json:"rows"`
}

// DrillCheck is one verdict of a drill: the restore itself, the tables and
// rows of a database, an assertion, the time limit or the cleanup.
type DrillCheck struct {
	Kind     string          `json:"kind"` // restore, tables, rows, sql, rto, cleanup
	Database string          `json:"database,omitempty"`
	Name     string          `json:"name"`
	Status   string          `json:"status"`
	Expected string          `json:"expected,omitempty"`
	Actual   string          `json:"actual,omitempty"`
	Message  string          `json:"message,omitempty"`
	Tables   []DrillTableRow `json:"tables,omitempty"` // tables whose rows differ
}

type DrillTableRow struct {
	Table    string `json:"table"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"` // -1: missing
}

// DrillRun is one execution of a drill.
type DrillRun struct {
	ID               int64        `json:"id"`
	DrillID          *int64       `json:"drill_id"`
	DrillName        string       `json:"drill_name"`
	JobID            *int64       `json:"job_id"`
	JobName          string       `json:"job_name"`
	RunID            *int64       `json:"run_id"`
	RunDir           string       `json:"run_dir"`
	TargetServerID   *int64       `json:"target_server_id"`
	TargetServerName string       `json:"target_server_name"`
	Status           string       `json:"status"` // running, passed, failed, canceled
	Trigger          string       `json:"trigger"`
	TriggeredBy      string       `json:"triggered_by"`
	StartedAt        time.Time    `json:"started_at"`
	FinishedAt       *time.Time   `json:"finished_at"`
	RestoreMs        int64        `json:"restore_ms"` // RTO: download and load
	CheckMs          int64        `json:"check_ms"`
	Databases        []DrillDB    `json:"databases"`
	Checks           []DrillCheck `json:"checks"`
	Summary          string       `json:"summary"`
	Error            string       `json:"error"`
	Leftover         []string     `json:"leftover"` // temporary databases still to drop
}

const drillCols = `d.id,d.job_id,COALESCE(j.name,''),d.name,d.target_server_id,COALESCE(s.name,''),d.schedule,d.enabled,d.databases,d.assertions,d.options,
d.notify_success,d.notify_failure,d.created_at,d.updated_at`

const drillFrom = ` FROM drills d LEFT JOIN jobs j ON j.id=d.job_id LEFT JOIN servers s ON s.id=d.target_server_id`

func scanDrill(row interface{ Scan(...any) error }) (*Drill, error) {
	var d Drill
	var en, ns, nf int
	var ca, ua int64
	var dbs, as, opts string
	if err := row.Scan(&d.ID, &d.JobID, &d.JobName, &d.Name, &d.TargetServerID, &d.TargetServerName, &d.Schedule, &en, &dbs, &as, &opts,
		&ns, &nf, &ca, &ua); err != nil {
		return nil, notFound(err)
	}
	fromJSON(dbs, &d.Databases)
	fromJSON(as, &d.Assertions)
	fromJSON(opts, &d.Options)
	if d.Databases == nil {
		d.Databases = []string{}
	}
	if d.Assertions == nil {
		d.Assertions = []DrillAssertion{}
	}
	d.Enabled, d.NotifySuccess, d.NotifyFailure = en == 1, ns == 1, nf == 1
	d.CreatedAt, d.UpdatedAt = fromTS(ca), fromTS(ua)
	return &d, nil
}

// ListDrills returns every drill, or those of one job (jobID > 0).
func (s *Store) ListDrills(jobID int64) ([]Drill, error) {
	q := `SELECT ` + drillCols + drillFrom
	var args []any
	if jobID > 0 {
		q += ` WHERE d.job_id=?`
		args = append(args, jobID)
	}
	rows, err := s.db.Query(q+` ORDER BY COALESCE(j.name,''), d.name, d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Drill{}
	for rows.Next() {
		d, err := scanDrill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *Store) GetDrill(id int64) (*Drill, error) {
	return scanDrill(s.db.QueryRow(`SELECT `+drillCols+drillFrom+` WHERE d.id=?`, id))
}

func (s *Store) CreateDrill(d *Drill) error {
	t := now()
	res, err := s.db.Exec(`INSERT INTO drills(job_id,name,target_server_id,schedule,enabled,databases,assertions,options,notify_success,notify_failure,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, d.JobID, d.Name, d.TargetServerID, d.Schedule, b2i(d.Enabled), toJSON(d.Databases), toJSON(d.Assertions), toJSON(d.Options),
		b2i(d.NotifySuccess), b2i(d.NotifyFailure), t, t)
	if err != nil {
		return err
	}
	d.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateDrill(d *Drill) error {
	_, err := s.db.Exec(`UPDATE drills SET job_id=?,name=?,target_server_id=?,schedule=?,enabled=?,databases=?,assertions=?,options=?,notify_success=?,notify_failure=?,updated_at=? WHERE id=?`,
		d.JobID, d.Name, d.TargetServerID, d.Schedule, b2i(d.Enabled), toJSON(d.Databases), toJSON(d.Assertions), toJSON(d.Options),
		b2i(d.NotifySuccess), b2i(d.NotifyFailure), now(), d.ID)
	return err
}

func (s *Store) DeleteDrill(id int64) error {
	_, err := s.db.Exec(`DELETE FROM drills WHERE id=?`, id)
	return err
}

// DrillsUsingServer counts the drills that restore into a server.
func (s *Store) DrillsUsingServer(id int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM drills WHERE target_server_id=?`, id).Scan(&n)
	return n
}

// ---- drill runs ----

const drillRunCols = `id,drill_id,drill_name,job_id,job_name,run_id,run_dir,target_server_id,target_server_name,status,trigger,triggered_by,
started_at,finished_at,restore_ms,check_ms,databases,checks,summary,error,leftover`

func scanDrillRun(row interface{ Scan(...any) error }) (*DrillRun, error) {
	var x DrillRun
	var did, jid, rid, tid, fin sql.NullInt64
	var st int64
	var dbs, checks, left string
	if err := row.Scan(&x.ID, &did, &x.DrillName, &jid, &x.JobName, &rid, &x.RunDir, &tid, &x.TargetServerName, &x.Status, &x.Trigger, &x.TriggeredBy,
		&st, &fin, &x.RestoreMs, &x.CheckMs, &dbs, &checks, &x.Summary, &x.Error, &left); err != nil {
		return nil, notFound(err)
	}
	x.DrillID, x.JobID, x.RunID, x.TargetServerID = fromNullID(did), fromNullID(jid), fromNullID(rid), fromNullID(tid)
	x.StartedAt, x.FinishedAt = fromTS(st), fromNullTS(fin)
	fromJSON(dbs, &x.Databases)
	fromJSON(checks, &x.Checks)
	fromJSON(left, &x.Leftover)
	if x.Databases == nil {
		x.Databases = []DrillDB{}
	}
	if x.Checks == nil {
		x.Checks = []DrillCheck{}
	}
	if x.Leftover == nil {
		x.Leftover = []string{}
	}
	return &x, nil
}

func (s *Store) CreateDrillRun(x *DrillRun) error {
	if x.StartedAt.IsZero() {
		x.StartedAt = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO drill_runs(drill_id,drill_name,job_id,job_name,run_id,run_dir,target_server_id,target_server_name,status,trigger,triggered_by,
		started_at,databases,checks,leftover) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nullID(x.DrillID), x.DrillName, nullID(x.JobID), x.JobName, nullID(x.RunID), x.RunDir, nullID(x.TargetServerID), x.TargetServerName, x.Status, x.Trigger, x.TriggeredBy,
		x.StartedAt.Unix(), toJSON(nonNil(x.Databases)), toJSON(nonNil(x.Checks)), toJSON(nonNil(x.Leftover)))
	if err != nil {
		return err
	}
	x.ID, _ = res.LastInsertId()
	return nil
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// UpdateDrillRun saves the progress or the result of a drill run.
func (s *Store) UpdateDrillRun(x *DrillRun) error {
	_, err := s.db.Exec(`UPDATE drill_runs SET status=?,finished_at=?,restore_ms=?,check_ms=?,databases=?,checks=?,summary=?,error=?,leftover=? WHERE id=?`,
		x.Status, nullTS(x.FinishedAt), x.RestoreMs, x.CheckMs, toJSON(nonNil(x.Databases)), toJSON(nonNil(x.Checks)), x.Summary, x.Error, toJSON(nonNil(x.Leftover)), x.ID)
	return err
}

// SetDrillLeftover records the temporary databases a drill still has to drop.
func (s *Store) SetDrillLeftover(id int64, names []string) {
	_, _ = s.db.Exec(`UPDATE drill_runs SET leftover=? WHERE id=?`, toJSON(nonNil(names)), id)
}

func (s *Store) GetDrillRun(id int64) (*DrillRun, error) {
	return scanDrillRun(s.db.QueryRow(`SELECT `+drillRunCols+` FROM drill_runs WHERE id=?`, id))
}

type DrillRunFilter struct {
	DrillID int64
	JobID   int64
	RunID   int64
	Limit   int
	Offset  int
}

func (s *Store) ListDrillRuns(f DrillRunFilter) ([]DrillRun, int, error) {
	var conds []string
	var args []any
	if f.DrillID > 0 {
		conds = append(conds, "drill_id=?")
		args = append(args, f.DrillID)
	}
	if f.JobID > 0 {
		conds = append(conds, "job_id=?")
		args = append(args, f.JobID)
	}
	if f.RunID > 0 {
		conds = append(conds, "run_id=?")
		args = append(args, f.RunID)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM drill_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, err := s.db.Query(`SELECT `+drillRunCols+` FROM drill_runs`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []DrillRun{}
	for rows.Next() {
		x, err := scanDrillRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *x)
	}
	return out, total, rows.Err()
}

// LastDrillRun returns the newest run of a drill (nil: never run).
func (s *Store) LastDrillRun(drillID int64) *DrillRun {
	list, _, err := s.ListDrillRuns(DrillRunFilter{DrillID: drillID, Limit: 1})
	if err != nil || len(list) == 0 {
		return nil
	}
	return &list[0]
}

// InterruptedDrills settles drill runs left running by a restart and returns
// them: their temporary databases may still exist on the sandbox.
func (s *Store) InterruptedDrills() []DrillRun {
	rows, err := s.db.Query(`SELECT ` + drillRunCols + ` FROM drill_runs WHERE status='running' OR leftover NOT IN ('','[]')`)
	if err != nil {
		return nil
	}
	var out []DrillRun
	for rows.Next() {
		if x, err := scanDrillRun(rows); err == nil {
			out = append(out, *x)
		}
	}
	rows.Close()
	_, _ = s.db.Exec(`UPDATE drill_runs SET status='failed', error='Interrupted by an application restart', finished_at=? WHERE status='running'`, now())
	return out
}

// PurgeDrillRuns removes drill history older than before and returns the ids.
func (s *Store) PurgeDrillRuns(before time.Time) ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM drill_runs WHERE status<>'running' AND started_at < ? AND leftover IN ('','[]')`, before.Unix())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_, _ = s.db.Exec(`DELETE FROM drill_runs WHERE id=?`, id)
	}
	return ids, nil
}
