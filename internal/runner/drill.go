package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"dumpcat/internal/dbconn"
	"dumpcat/internal/engine"
	"dumpcat/internal/notify"
	"dumpcat/internal/store"
	"dumpcat/internal/tasklog"
)

// ---- restore drills ----
//
// A drill restores a job's newest backup into a sandbox server under
// temporary names (<db>__drill_<id>), compares the tables and the rows of
// every table with what the backup recorded, runs the drill's assertions,
// records how long the restore took (RTO) and drops the copies again. It
// reuses the logical restore (renamed databases, never DROP of an existing
// database) and only ever drops the databases it created itself.

var ErrDrillRunning = errors.New("this drill is already running")

const drillMarker = "__drill_"

func (r *Runner) DrillLogPath(id int64) string {
	return filepath.Join(r.DataDir, "logs", fmt.Sprintf("drill-%d.log", id))
}

// drillTempName names the copy of database db made by drill run id: the name
// plus "__drill_<id>", cut to MySQL's 64 characters. taken holds the names
// already used (lower case): every database of the backup and earlier copies.
func drillTempName(db string, id int64, taken map[string]bool) string {
	base := []rune(db)
	for n := 0; ; n++ {
		suffix := fmt.Sprintf("%s%d", drillMarker, id)
		if n > 0 {
			suffix += fmt.Sprintf("_%d", n)
		}
		b := base
		if keep := 64 - len([]rune(suffix)); len(b) > keep {
			b = b[:keep]
		}
		name := string(b) + suffix
		if !taken[strings.ToLower(name)] {
			taken[strings.ToLower(name)] = true
			return name
		}
	}
}

// isDrillTemp tells whether name is a copy made by drill run id: the only
// databases a drill ever drops.
func isDrillTemp(name string, id int64) bool {
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(fmt.Sprintf("%s%d", drillMarker, id)) + `(_\d+)?$`)
	return re.MatchString(name)
}

// drillRunnable tells why a backup cannot be used by a drill.
func drillRunnable(run *store.Run) error {
	switch {
	case run.Status != store.StatusSuccess && run.Status != store.StatusPartial:
		return fmt.Errorf("backup #%d is not a successful backup", run.ID)
	case run.Method == store.MethodPhysical:
		return errors.New("restore drills test logical backups (mysqldump, mydumper); physical backups are not supported yet")
	case run.Meta["single_file"] == true:
		return errors.New("a single-file dump restores under its original database names, so it cannot be drilled under temporary names — turn off \"One file for all databases\" in the job")
	case run.Encrypted && run.Passphrase == "":
		return errors.New("the backup is encrypted and its passphrase is not stored in DumpCat")
	}
	for _, l := range run.Locations {
		if l.Status == store.LocOK && l.StorageID != nil {
			return nil
		}
	}
	return errors.New("no available storage location for this backup")
}

// drillCandidate picks the newest backup of a job that a drill can restore.
func (r *Runner) drillCandidate(job *store.Job) (*store.Run, error) {
	runs, err := r.Store.RetentionCandidates(job.ID)
	if err != nil {
		return nil, err
	}
	var first error
	for i := range runs {
		run := &runs[i]
		err := drillRunnable(run)
		if err == nil {
			return run, nil
		}
		if first == nil {
			first = fmt.Errorf("the newest backup (#%d) cannot be drilled: %w", run.ID, err)
		}
		if run.Method == store.MethodPhysical || run.Meta["single_file"] == true {
			return nil, first // every backup of this job is alike
		}
	}
	if first != nil {
		return nil, first
	}
	return nil, errors.New("the job has no successful backup yet")
}

// CheckDrillJob tells why the backups of a job cannot be drilled ("" when they can).
func CheckDrillJob(job *store.Job) string {
	switch {
	case job.Method == store.MethodPhysical:
		return "restore drills test logical backups (mysqldump, mydumper); physical backups are not supported yet"
	case job.Method == store.MethodMysqldump && job.Options.SingleFile:
		return "the job writes one file for all databases, which restores under the original names only — turn off \"One file for all databases\" to drill its backups"
	}
	return ""
}

func (r *Runner) DrillRunning(drillID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drills[drillID]
}

// StartDrill launches a drill on runID, or on the newest backup of the job
// that can be restored (runID 0).
func (r *Runner) StartDrill(drillID, runID int64, trigger, user string) (*store.DrillRun, error) {
	if err := r.beginStart(); err != nil {
		return nil, err
	}
	defer r.endStart()
	d, err := r.Store.GetDrill(drillID)
	if err != nil {
		return nil, err
	}
	job, err := r.Store.GetJob(d.JobID)
	if err != nil {
		return nil, fmt.Errorf("job: %w", err)
	}
	target, err := r.Store.GetServer(d.TargetServerID)
	if err != nil {
		return nil, fmt.Errorf("sandbox server: %w", err)
	}
	target = target.ForRestore()
	var run *store.Run
	if runID > 0 {
		if run, err = r.Store.GetRun(runID); err != nil {
			return nil, err
		}
		if run.JobUUID != job.UUID {
			return nil, errors.New("this backup belongs to another job")
		}
		if err := drillRunnable(run); err != nil {
			return nil, err
		}
	} else if run, err = r.drillCandidate(job); err != nil {
		return nil, err
	}
	var dbs []store.DBEntry
	want := map[string]bool{}
	for _, n := range d.Databases {
		want[n] = true
	}
	for _, e := range run.Databases {
		if len(want) == 0 || want[e.Name] {
			dbs = append(dbs, e)
		}
	}
	if len(dbs) == 0 {
		return nil, errors.New("none of the drill's databases is in this backup")
	}
	r.mu.Lock()
	if r.drills[d.ID] {
		r.mu.Unlock()
		return nil, ErrDrillRunning
	}
	r.drills[d.ID] = true
	r.mu.Unlock()
	done := func() {
		r.mu.Lock()
		delete(r.drills, d.ID)
		r.mu.Unlock()
	}

	did, jid, rid, tid := d.ID, job.ID, run.ID, target.ID
	x := &store.DrillRun{DrillID: &did, DrillName: d.Name, JobID: &jid, JobName: job.Name, RunID: &rid, RunDir: run.DirName,
		TargetServerID: &tid, TargetServerName: target.Name, Status: store.StatusRunning, Trigger: trigger, TriggeredBy: user}
	if err := r.Store.CreateDrillRun(x); err != nil {
		done()
		return nil, err
	}
	// the copies never take a name of the backup's own databases
	taken := map[string]bool{}
	for _, e := range run.Databases {
		taken[strings.ToLower(e.Name)] = true
	}
	for _, e := range dbs {
		x.Databases = append(x.Databases, store.DrillDB{Name: e.Name, Temp: drillTempName(e.Name, x.ID, taken)})
	}
	if err := r.Store.UpdateDrillRun(x); err != nil {
		done()
		return nil, err
	}
	release := r.useRuns(run.ID)
	task, ctx := r.addTask("drill", x.ID, job.ID, fmt.Sprintf("%s: %s %s → %s", d.Name, job.Name, run.DirName, target.Name))
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer done()
		defer release()
		defer r.removeTask(task)
		r.execDrill(ctx, task, x, d, run, target)
	}()
	return x, nil
}

func (r *Runner) execDrill(ctx context.Context, task *Task, x *store.DrillRun, d *store.Drill, run *store.Run, target *store.Server) {
	lg, err := tasklog.Open(r.DrillLogPath(x.ID))
	if err != nil {
		lg = tasklog.Discard()
	}
	defer lg.Close()
	lg.Redact(target.Password, run.Passphrase)
	start := time.Now()
	lg.Printf("DumpCat %s — RESTORE DRILL \"%s\": backup #%d \"%s\" %s (%s) → sandbox %s", r.AppVersion, d.Name, run.ID, run.JobName, run.DirName, run.Method, target.Name)
	var checks []store.DrillCheck
	fail := func(err error) { r.finishDrill(x, d, run, lg, start, err, checks) }

	lg.Printf("Waiting for an execution slot...")
	if err := r.acquire(ctx); err != nil {
		fail(err)
		return
	}
	defer r.release()
	task.SetPhase("prepare")
	same := r.sameServer(run, target)
	noBinlog := d.Options.NoBinlog || same
	if same {
		lg.Printf("The sandbox is the backup's own source server: the copies get temporary names that cannot collide with its databases, and nothing is written to its binary log")
	}
	info, err := r.drillPreflight(ctx, target, x, lg)
	if err != nil {
		fail(err)
		return
	}
	// recorded before anything is created, so a restart can still drop them
	x.Leftover = nil
	for _, m := range x.Databases {
		x.Leftover = append(x.Leftover, m.Temp)
	}
	r.Store.SetDrillLeftover(x.ID, x.Leftover)
	defer func() {
		task.SetPhase("cleanup")
		if c := r.dropDrillDatabases(target, x, noBinlog, lg); c != nil {
			checks = append(checks, *c)
		}
		r.finishDrill(x, d, run, lg, start, err, checks)
	}()

	req := RestoreRequest{Rename: map[string]string{}, NoBinlog: noBinlog, Threads: d.Options.Threads}
	for _, m := range x.Databases {
		req.Databases = append(req.Databases, m.Name)
		req.Rename[m.Name] = m.Temp
	}
	_, sug := compatNotes(run, targetFlavorOf(target, info), info.Version, same)
	req.StripDefiner, req.ConvertCollations, req.SkipGTIDPurged = sug.StripDefiner, sug.ConvertCollations, sug.SkipGTIDPurged
	for _, m := range x.Databases {
		lg.Printf("  %s → %s", m.Name, m.Temp)
	}
	workDir := filepath.Join(r.stagingRoot(), fmt.Sprintf("work-drill-%d", x.ID))
	defer os.RemoveAll(workDir)
	if err = os.MkdirAll(workDir, 0o700); err != nil {
		return
	}
	env := &engine.Env{Ctx: ctx, Log: lg, Tools: r.Tools, TmpDir: workDir}
	lg.Printf("=== RESTORE ===")
	t0 := time.Now()
	if run.Method == store.MethodMydumper {
		err = r.restoreMydumper(ctx, task, env, run, target, req, run.Passphrase, workDir)
	} else {
		err = r.restoreMysqldump(ctx, task, env, run, target, req, run.Passphrase)
	}
	rto := time.Since(t0)
	x.RestoreMs = rto.Milliseconds()
	if err != nil {
		if same && noBinlog && strings.Contains(err.Error(), "not allowed to disable binlog") {
			err = fmt.Errorf("%w — a drill on the backup's own source server must stay out of its binary log (replicas, binlog archive)", err)
		}
		checks = append(checks, store.DrillCheck{Kind: "restore", Name: "Restore", Status: store.CheckFailed, Message: err.Error()})
		return
	}
	lg.Printf("Restored in %s", humanDuration(rto))
	checks = append(checks, store.DrillCheck{Kind: "restore", Name: "Restore", Status: store.CheckOK, Actual: humanDuration(rto),
		Message: fmt.Sprintf("%d database(s) restored in %s", len(x.Databases), humanDuration(rto))})
	if max := d.Options.MaxRTOMinutes; max > 0 {
		c := store.DrillCheck{Kind: "rto", Name: "Restore time (RTO)", Status: store.CheckOK, Expected: fmt.Sprintf("≤ %d min", max), Actual: humanDuration(rto)}
		if rto > time.Duration(max)*time.Minute {
			c.Status, c.Message = store.CheckFailed, fmt.Sprintf("the restore took %s, longer than the %d minutes allowed", humanDuration(rto), max)
		}
		checks = append(checks, c)
	}
	task.SetPhase("check")
	lg.Printf("=== CHECKS ===")
	t1 := time.Now()
	checks = append(checks, r.drillChecks(ctx, lg, d, run, target, x)...)
	x.CheckMs = time.Since(t1).Milliseconds()
	err = ctx.Err()
}

// drillPreflight connects to the sandbox and makes sure none of the
// temporary names exists there: a drill only ever drops what it created.
func (r *Runner) drillPreflight(ctx context.Context, target *store.Server, x *store.DrillRun, lg *tasklog.Logger) (*dbconn.ServerInfo, error) {
	info, err := dbconn.Inspect(ctx, target, false)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the sandbox with its restore account: %w", err)
	}
	have := map[string]bool{}
	for _, d := range info.Databases {
		have[strings.ToLower(d.Name)] = true
	}
	for _, m := range x.Databases {
		if have[strings.ToLower(m.Temp)] {
			return nil, fmt.Errorf("database %s already exists on the sandbox; a drill never touches databases it did not create — drop or rename it, then run the drill again", m.Temp)
		}
	}
	lg.Printf("Sandbox: %s — %s %s on %s", target.Name, targetFlavorOf(target, info), info.Version, info.Hostname)
	return info, nil
}

// dropDrillDatabases drops the temporary copies of a drill run. It returns a
// cleanup check when something is left behind (nil: all gone).
func (r *Runner) dropDrillDatabases(target *store.Server, x *store.DrillRun, noBinlog bool, lg *tasklog.Logger) *store.DrillCheck {
	if len(x.Leftover) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var left, errs []string
	err := func() error {
		db, err := dbconn.Open(ctx, target, "")
		if err != nil {
			return err
		}
		defer db.Close()
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if noBinlog {
			if _, err := conn.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
				lg.Printf("WARNING: could not disable the binary log for the cleanup: %v", err)
			}
		}
		for _, name := range x.Leftover {
			if !isDrillTemp(name, x.ID) {
				lg.Printf("WARNING: refusing to drop %s: not a database of this drill", name)
				continue
			}
			lg.Printf("DROP DATABASE IF EXISTS %s", dbconn.QuoteIdent(name))
			if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbconn.QuoteIdent(name)); err != nil {
				left = append(left, name)
				errs = append(errs, name+": "+err.Error())
			}
		}
		return nil
	}()
	if err != nil {
		left, errs = x.Leftover, []string{err.Error()}
	}
	x.Leftover = left
	r.Store.SetDrillLeftover(x.ID, left)
	if len(left) == 0 {
		lg.Printf("Temporary databases dropped")
		return nil
	}
	msg := "could not drop " + strings.Join(left, ", ") + " on the sandbox: " + strings.Join(errs, "; ")
	lg.Printf("WARNING: %s", msg)
	return &store.DrillCheck{Kind: "cleanup", Name: "Cleanup", Status: store.CheckWarning, Message: msg}
}

// drillChecks compares the restored copies with the backup and runs the
// drill's assertions.
func (r *Runner) drillChecks(ctx context.Context, lg *tasklog.Logger, d *store.Drill, run *store.Run, target *store.Server, x *store.DrillRun) []store.DrillCheck {
	var out []store.DrillCheck
	stats, _ := r.Store.RunTables(run.ID)
	db, err := dbconn.Open(ctx, target, "")
	if err != nil {
		return []store.DrillCheck{{Kind: "tables", Name: "Tables", Status: store.CheckFailed, Message: "could not connect to the sandbox: " + err.Error()}}
	}
	defer db.Close()
	var lctn int
	_ = db.QueryRowContext(ctx, "SELECT @@lower_case_table_names").Scan(&lctn)
	fold := func(s string) string {
		if lctn != 0 {
			return strings.ToLower(s)
		}
		return s
	}
	entries := map[string]store.DBEntry{}
	for _, e := range run.Databases {
		entries[e.Name] = e
	}
	for i := range x.Databases {
		m := &x.Databases[i]
		got, err := restoredTables(ctx, db, fold(m.Temp))
		if err != nil {
			out = append(out, store.DrillCheck{Kind: "tables", Database: m.Name, Name: "Tables", Status: store.CheckFailed, Message: err.Error()})
			continue
		}
		have := map[string]string{} // folded → real name
		for _, t := range got {
			have[fold(t)] = t
		}
		m.Tables = len(got)
		recorded, ok := stats[m.Name]
		if !ok {
			// a backup from before row counting: compare with information_schema at backup time
			all, err := countSchemaObjects(ctx, db, fold(m.Temp))
			c := store.DrillCheck{Kind: "tables", Database: m.Name, Name: "Tables", Status: store.CheckOK, Expected: strconv.Itoa(entries[m.Name].Tables), Actual: strconv.Itoa(all),
				Message: "tables and views, as listed by the server when the backup was made"}
			if err != nil {
				c.Status, c.Message = store.CheckFailed, err.Error()
			} else if all != entries[m.Name].Tables {
				c.Status = store.CheckWarning
				c.Message = fmt.Sprintf("the server listed %d tables and views when the backup was made, the copy has %d (the backup records no exact table list: excluded tables or a change during the dump explain a difference)", entries[m.Name].Tables, all)
			}
			out = append(out, c)
			out = append(out, store.DrillCheck{Kind: "rows", Database: m.Name, Name: "Row counts", Status: store.CheckSkipped,
				Message: "this backup was made before DumpCat counted rows: run a new backup to compare row counts"})
			lg.Printf("%s: %d tables (backup without row counts)", m.Name, m.Tables)
			continue
		}
		names := make([]string, 0, len(recorded))
		for t := range recorded {
			names = append(names, t)
		}
		sort.Strings(names)
		var missing, extra []string
		want := map[string]bool{}
		for _, t := range names {
			want[fold(t)] = true
			if _, ok := have[fold(t)]; !ok {
				missing = append(missing, t)
			}
		}
		for _, t := range got {
			if !want[fold(t)] {
				extra = append(extra, t)
			}
		}
		tc := store.DrillCheck{Kind: "tables", Database: m.Name, Name: "Tables", Status: store.CheckOK, Expected: strconv.Itoa(len(recorded)), Actual: strconv.Itoa(len(got))}
		if len(missing)+len(extra) > 0 {
			tc.Status = store.CheckFailed
			var parts []string
			if len(missing) > 0 {
				parts = append(parts, "missing: "+listSome(missing, 10))
			}
			if len(extra) > 0 {
				parts = append(parts, "not in the backup: "+listSome(extra, 10))
			}
			tc.Message = strings.Join(parts, "; ")
		}
		out = append(out, tc)
		rc := store.DrillCheck{Kind: "rows", Database: m.Name, Name: "Row counts", Status: store.CheckOK}
		var expTotal, gotTotal int64
		var bad []store.DrillTableRow
		for _, t := range names {
			exp := recorded[t]
			expTotal += exp
			real, ok := have[fold(t)]
			if !ok {
				bad = append(bad, store.DrillTableRow{Table: t, Expected: exp, Actual: -1})
				continue
			}
			var n int64
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dbconn.QuoteIdent(fold(m.Temp))+"."+dbconn.QuoteIdent(real)).Scan(&n); err != nil {
				if ctx.Err() != nil {
					return out
				}
				bad = append(bad, store.DrillTableRow{Table: t, Expected: exp, Actual: -1})
				lg.Printf("  ✗ %s.%s: %v", m.Name, t, err)
				continue
			}
			gotTotal += n
			if n != exp {
				bad = append(bad, store.DrillTableRow{Table: t, Expected: exp, Actual: n})
			}
		}
		m.Rows = gotTotal
		rc.Expected, rc.Actual = humanCount(expTotal), humanCount(gotTotal)
		rc.Message = fmt.Sprintf("%d table(s), every row count matches the backup", len(names))
		if len(bad) > 0 {
			rc.Status = store.CheckFailed
			rc.Message = fmt.Sprintf("%d of %d table(s) differ from the backup", len(bad), len(names))
			if len(bad) > 200 {
				bad = bad[:200]
			}
			rc.Tables = bad
		}
		out = append(out, rc)
		lg.Printf("%s: %d tables %s, %s rows %s", m.Name, len(got), mark(tc.Status), humanCount(gotTotal), mark(rc.Status))
		for _, b := range bad {
			if b.Actual < 0 {
				lg.Printf("  ✗ %s: missing (backup: %d rows)", b.Table, b.Expected)
			} else {
				lg.Printf("  ✗ %s: %d rows, backup: %d", b.Table, b.Actual, b.Expected)
			}
		}
	}
	for _, a := range d.Assertions {
		c := r.drillAssert(ctx, target, a, x)
		lg.Printf("Assertion %q: %s %s", a.Name, mark(c.Status), c.Message)
		out = append(out, c)
	}
	return out
}

func mark(status string) string {
	switch status {
	case store.CheckOK:
		return "✓"
	case store.CheckFailed:
		return "✗"
	}
	return "–"
}

func listSome(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:n], ", ") + fmt.Sprintf(" and %d more", len(list)-n)
}

func humanCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// restoredTables lists the base tables of a database.
func restoredTables(ctx context.Context, db *sql.DB, schema string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_TYPE IN ('BASE TABLE','SYSTEM VERSIONED')`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func countSchemaObjects(ctx context.Context, db *sql.DB, schema string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=?`, schema).Scan(&n)
	return n, err
}

var reAssertStart = regexp.MustCompile(`(?is)^[\s(]*(SELECT|WITH)\b`)
var reAssertFile = regexp.MustCompile(`(?i)\bINTO\s+(OUTFILE|DUMPFILE)\b`)

// ValidateAssertion accepts one read-only query (SELECT or WITH).
func ValidateAssertion(q string) error {
	q = strings.TrimRight(strings.TrimSpace(q), "; \t\r\n")
	switch {
	case q == "":
		return errors.New("the query is empty")
	case !reAssertStart.MatchString(q):
		return errors.New("an assertion must be one SELECT (or WITH … SELECT) query")
	case strings.Contains(q, ";"):
		return errors.New("an assertion must be a single statement")
	case reAssertFile.MatchString(q):
		return errors.New("an assertion cannot write files")
	}
	return nil
}

// truthy is how an assertion's answer is read: NULL, 0, "" and "false" fail.
func truthy(v sql.NullString) bool {
	if !v.Valid {
		return false
	}
	s := strings.TrimSpace(v.String)
	if s == "" || strings.EqualFold(s, "false") {
		return false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f != 0
	}
	return true
}

// drillAssert runs one assertion in a read-only session of its own, inside
// the temporary copy of its database.
func (r *Runner) drillAssert(ctx context.Context, target *store.Server, a store.DrillAssertion, x *store.DrillRun) store.DrillCheck {
	c := store.DrillCheck{Kind: "sql", Database: a.Database, Name: a.Name, Status: store.CheckFailed, Expected: "true"}
	if c.Name == "" {
		c.Name = "Assertion"
	}
	q := strings.TrimRight(strings.TrimSpace(a.SQL), "; \t\r\n")
	if err := ValidateAssertion(q); err != nil {
		c.Message = err.Error()
		return c
	}
	schema := ""
	if a.Database != "" {
		for _, m := range x.Databases {
			if m.Name == a.Database {
				schema = m.Temp
			}
		}
		if schema == "" {
			c.Message = fmt.Sprintf("database %s is not part of this drill", a.Database)
			return c
		}
	}
	actx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	db, err := dbconn.Open(actx, target, schema)
	if err != nil {
		c.Message = err.Error()
		return c
	}
	defer db.Close()
	conn, err := db.Conn(actx)
	if err != nil {
		c.Message = err.Error()
		return c
	}
	defer conn.Close()
	_, _ = conn.ExecContext(actx, "SET SESSION TRANSACTION READ ONLY")
	_, _ = conn.ExecContext(actx, "SET SESSION max_statement_time=600")    // MariaDB
	_, _ = conn.ExecContext(actx, "SET SESSION max_execution_time=600000") // MySQL
	rows, err := conn.QueryContext(actx, q)
	if err != nil {
		c.Message = err.Error()
		return c
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || len(cols) == 0 {
		c.Message = "the query returned no column"
		return c
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			c.Message = err.Error()
		} else {
			c.Message, c.Actual = "the query returned no row", "no row"
		}
		return c
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		c.Message = err.Error()
		return c
	}
	c.Actual = "NULL"
	if vals[0].Valid {
		c.Actual = vals[0].String
		if r := []rune(c.Actual); len(r) > 200 {
			c.Actual = string(r[:199]) + "…"
		}
	}
	if truthy(vals[0]) {
		c.Status, c.Message = store.CheckOK, "the first column is true"
	} else {
		c.Message = fmt.Sprintf("the first column is %s, not true", c.Actual)
	}
	return c
}

func (r *Runner) finishDrill(x *store.DrillRun, d *store.Drill, run *store.Run, lg *tasklog.Logger, start time.Time, err error, checks []store.DrillCheck) {
	now := time.Now()
	x.FinishedAt = &now
	x.Checks = checks
	var failed, warned []string
	passed := 0
	for _, c := range checks {
		label := c.Name
		if c.Database != "" && c.Kind != "sql" {
			label = c.Database + ": " + c.Name
		}
		switch c.Status {
		case store.CheckOK:
			passed++
		case store.CheckFailed:
			failed = append(failed, label+" — "+c.Message)
		case store.CheckWarning:
			if c.Kind == "cleanup" {
				warned = append(warned, c.Message)
			}
		}
	}
	x.Status, x.Error = store.DrillPassed, ""
	switch {
	case errors.Is(err, context.Canceled):
		x.Status, x.Error = store.StatusCanceled, "Canceled"
	case len(failed) > 0:
		x.Status, x.Error = store.DrillFailed, strings.Join(failed, "\n")
	case err != nil:
		x.Status, x.Error = store.DrillFailed, err.Error()
	}
	var tables int
	var rows int64
	for _, m := range x.Databases {
		tables += m.Tables
		rows += m.Rows
	}
	parts := []string{fmt.Sprintf("%d database(s)", len(x.Databases))}
	if tables > 0 {
		parts = append(parts, fmt.Sprintf("%d tables", tables), humanCount(rows)+" rows")
	}
	if x.RestoreMs > 0 {
		parts = append(parts, "restored in "+humanDuration(time.Duration(x.RestoreMs)*time.Millisecond))
	}
	parts = append(parts, fmt.Sprintf("%d/%d checks passed", passed, len(checks)))
	x.Summary = strings.Join(parts, " · ")
	if len(warned) > 0 && x.Status == store.DrillPassed {
		x.Error = "Cleanup: " + strings.Join(warned, "; ")
	}
	_ = r.Store.UpdateDrillRun(x)
	// the backup is marked only once its restore was attempted: an unreachable
	// sandbox or a name already taken there says nothing about the backup
	if (x.Status == store.DrillPassed || x.Status == store.DrillFailed) && len(checks) > 0 {
		r.Store.SetRunDrill(run.ID, x.Status, x.Summary)
	}
	switch x.Status {
	case store.DrillPassed:
		lg.Printf("DRILL PASSED in %s — %s", humanDuration(time.Since(start)), x.Summary)
	case store.StatusCanceled:
		lg.Printf("DRILL CANCELED")
	default:
		lg.Printf("DRILL FAILED: %s", strings.ReplaceAll(x.Error, "\n", "; "))
	}
	if r.Notifier == nil || x.Status == store.StatusCanceled {
		return
	}
	status, title := "success", "Restore drill passed: "+run.JobName
	switch {
	case x.Status == store.DrillFailed:
		status, title = "failed", "Restore drill FAILED: "+run.JobName
	case len(warned) > 0:
		status, title = "partial", "Restore drill passed, cleanup failed: "+run.JobName
	}
	if (status == "success" && !d.NotifySuccess) || (status != "success" && !d.NotifyFailure) {
		return
	}
	ev := notify.Event{Kind: "drill", Status: status, Title: title, Error: x.Error, URL: r.link(fmt.Sprintf("/#/drill-runs/%d", x.ID))}
	ev.Add("Drill", d.Name)
	ev.Add("Backup", fmt.Sprintf("#%d %s", run.ID, run.DirName))
	ev.Add("Sandbox", x.TargetServerName)
	names := make([]string, len(x.Databases))
	for i, m := range x.Databases {
		names[i] = m.Name
	}
	ev.Add("Databases", strings.Join(names, ", "))
	if x.RestoreMs > 0 {
		ev.Add("Restore time (RTO)", humanDuration(time.Duration(x.RestoreMs)*time.Millisecond))
	}
	ev.Add("Checks", fmt.Sprintf("%d passed, %d failed", passed, len(failed)))
	r.Notifier.Send(ev)
}

// recoverDrills drops the temporary databases of drills interrupted by a
// restart (in the background).
func (r *Runner) recoverDrills() {
	list := r.Store.InterruptedDrills()
	if len(list) == 0 {
		return
	}
	go func() {
		for i := range list {
			x := &list[i]
			if len(x.Leftover) == 0 || x.TargetServerID == nil {
				continue
			}
			sv, err := r.Store.GetServer(*x.TargetServerID)
			if err != nil {
				continue
			}
			lg, err := tasklog.Open(r.DrillLogPath(x.ID))
			if err != nil {
				lg = tasklog.Discard()
			}
			lg.Printf("Dropping the temporary databases left by the interrupted drill")
			if c := r.dropDrillDatabases(sv.ForRestore(), x, true, lg); c == nil {
				log.Printf("drill #%d: dropped the temporary databases left on %s", x.ID, sv.Name)
			} else {
				log.Printf("drill #%d: %s", x.ID, c.Message)
			}
			lg.Close()
		}
	}()
}

// DrillOnSource tells whether a sandbox is the server a job backs up.
func (r *Runner) DrillOnSource(job *store.Job, sv *store.Server) bool {
	sid := job.ServerID
	return r.sameServer(&store.Run{ServerID: &sid}, sv)
}
